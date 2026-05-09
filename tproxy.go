package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/txthinking/socks5"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

var (
	socksClientCache = make(map[string]*socks5.Client)
	udpClientMu      sync.RWMutex
)

// 獲取或創建 SOCKS5 客戶端實例
func getSocksClient(proxyAddr string) (*socks5.Client, error) {
	udpClientMu.RLock()
	c, exists := socksClientCache[proxyAddr]
	udpClientMu.RUnlock()
	if exists {
		return c, nil
	}

	udpClientMu.Lock()
	defer udpClientMu.Unlock()

	user, pass, addr := parseSocksAddr(proxyAddr)
	// 緩存實例，避免重複解析地址和分配內存
	c, err := socks5.NewClient(addr, user, pass, 10, 10)
	if err == nil {
		socksClientCache[proxyAddr] = c
	}
	return c, err
}

func setTransparentSocket(network, address string, c syscall.RawConn) error {
	var err error
	c.Control(func(fd uintptr) {
		err = unix.SetsockoptInt(int(fd), unix.SOL_IPV6, unix.IPV6_TRANSPARENT, 1)
		if network == "udp" || network == "udp4" || network == "udp6" {
			unix.SetsockoptInt(int(fd), unix.SOL_IPV6, unix.IPV6_RECVORIGDSTADDR, 1)
		}
	})
	return err
}

func startTCPTProxy(ctx context.Context, addr string) {
	lc := net.ListenConfig{Control: setTransparentSocket}
	listener, err := lc.Listen(context.Background(), "tcp6", addr)
	if err != nil {
		zap.S().Fatalf("TCP TProxy 失敗: %v", err)
	}
	zap.S().Infof("TCP TProxy 啟動於 %s", addr)

	go func() {
		<-ctx.Done()
		zap.S().Infof("[TCP] 正在關閉 TProxy 監聽器...")
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		go handleTCP(conn)
	}
}

func handleTCP(clientConn net.Conn) {
	defer clientConn.Close()
	targetAddr := clientConn.LocalAddr().(*net.TCPAddr)
	domain, ok := pool.LookUp(targetAddr.IP.String())
	if !ok {
		return
	}

	targetEndpoint := fmt.Sprintf("%s:%d", strings.TrimSuffix(domain, "."), targetAddr.Port)

	node := router.MatchNode(domain)
	upstreamProxy := cfg.Routing.DefaultUpstream
	var rewrites map[string]string

	if node != nil && node.Upstream != "" {
		upstreamProxy = node.Upstream
		rewrites = node.HeaderRewrite
	}

	isDirect := upstreamProxy == "" || strings.ToUpper(upstreamProxy) == "DIRECT"
	isReject := strings.ToUpper(upstreamProxy) == "REJECT"

	if isReject {
		zap.S().Debugf("[TCP] 攔截請求: %s (命中 REJECT)", domain)
		return
	}

	var targetConn net.Conn
	var err error

	if !isDirect {
		client, cErr := getSocksClient(upstreamProxy)
		if cErr != nil {
			zap.S().Errorf("[TCP] SOCKS5 客戶端初始化失敗: %v", cErr)
			return
		}
		zap.S().Debugf("[TCP] 匹配代理: %s -> %s", domain, upstreamProxy)
		targetConn, err = client.Dial("tcp", targetEndpoint)

		if err != nil && strings.Contains(err.Error(), "address type not supported") {
			zap.S().Warnf("[TCP] 上遊不支持域名解析，嘗試本地解析後發送 IP: %s", domain)
			ips, _ := defaultResolver.LookupIP(domain)
			if len(ips) > 0 {
				targetConn, err = client.Dial("tcp", fmt.Sprintf("%s:%d", ips[0].String(), targetAddr.Port))
			}
		}
	} else {
		zap.S().Debugf("[TCP] 匹配直連: %s", domain)
		ips, resolveErr := defaultResolver.LookupIP(domain)
		if resolveErr != nil || len(ips) == 0 {
			zap.S().Warnf("[TCP] 直連解析 %s 失敗: %v", domain, resolveErr)
			return
		}

		for _, ip := range ips {
			addr := fmt.Sprintf("%s:%d", ip.String(), targetAddr.Port)
			targetConn, err = net.DialTimeout("tcp", addr, 3*time.Second)
			if err == nil {
				zap.S().Debugf("[TCP] 直連撥號成功: %s -> %s", domain, addr)
				break
			}
		}

		if targetConn == nil {
			zap.S().Warnf("[TCP] 直連 %s 所有 IP 均失敗", domain)
			return
		}
	}

	if err != nil || targetConn == nil {
		zap.S().Errorf("[TCP] 無法連接到上游 %s: %v", domain, err)
		return
	}
	defer targetConn.Close()

	if targetAddr.Port == 80 && len(rewrites) > 0 {
		reader := bufio.NewReader(clientConn)
		newHeader, err := rewriteHTTPHeader(reader, rewrites)
		if err == nil {
			targetConn.Write(newHeader)
			go io.Copy(targetConn, reader)
			io.Copy(clientConn, targetConn)
			return
		}
	}

	go io.Copy(targetConn, clientConn)
	io.Copy(clientConn, targetConn)
}

type UDPSession struct {
	UpstreamConn net.Conn
	LastActive   time.Time
}

var (
	udpSessions = make(map[string]*UDPSession)
	sessionMu   sync.RWMutex
)

func startUDPTProxy(ctx context.Context, addr string) {
	lc := net.ListenConfig{Control: setTransparentSocket}
	packetConn, err := lc.ListenPacket(context.Background(), "udp6", addr)
	if err != nil {
		zap.S().Fatalf("UDP TProxy 失敗: %v", err)
	}
	zap.S().Infof("UDP TProxy 啟動於 %s", addr)
	udpConn := packetConn.(*net.UDPConn)

	go func() {
		<-ctx.Done()
		zap.S().Infof("[UDP] 正在關閉 TProxy 監聽器...")
		udpConn.Close()
	}()

	buf := make([]byte, 65536)
	oob := make([]byte, 1024)

	for {
		n, oobn, _, clientAddr, err := udpConn.ReadMsgUDP(buf, oob)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		fakeIPAddr, err := parseIPv6OriginalDst(oob[:oobn])
		if err != nil {
			continue
		}
		payload := make([]byte, n)
		copy(payload, buf[:n])
		go handleUDP(payload, clientAddr, fakeIPAddr)
	}
}

func parseIPv6OriginalDst(oob []byte) (*net.UDPAddr, error) {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	for _, msg := range msgs {
		if msg.Header.Level == unix.SOL_IPV6 && msg.Header.Type == unix.IPV6_RECVORIGDSTADDR {
			if len(msg.Data) >= 28 {
				port := int(msg.Data[2])<<8 | int(msg.Data[3])
				ip := net.IP(msg.Data[8:24])
				return &net.UDPAddr{IP: ip, Port: port}, nil
			}
		}
	}
	return nil, fmt.Errorf("no dest")
}

func handleUDP(payload []byte, clientAddr *net.UDPAddr, fakeIP *net.UDPAddr) {
	sessionKey := fmt.Sprintf("%s_%s", clientAddr.String(), fakeIP.String())

	sessionMu.RLock()
	sess, exists := udpSessions[sessionKey]
	sessionMu.RUnlock()

	if !exists {
		domain, ok := pool.LookUp(fakeIP.IP.String())
		if !ok {
			return
		}

		node := router.MatchNode(domain)
		upstreamProxy := cfg.Routing.DefaultUpstream
		if node != nil && node.Upstream != "" {
			upstreamProxy = node.Upstream
		}

		isDirect := upstreamProxy == "" || strings.ToUpper(upstreamProxy) == "DIRECT"
		isReject := strings.ToUpper(upstreamProxy) == "REJECT"

		if isReject {
			zap.S().Debugf("[UDP] 攔截請求: %s (命中 REJECT)", domain)
			return
		}

		var upstreamConn net.Conn
		var err error

		if !isDirect {
			client, cErr := getSocksClient(upstreamProxy)
			if cErr != nil {
				zap.S().Errorf("[UDP] SOCKS5 客戶端初始化失敗: %v", cErr)
				return
			}
			zap.S().Debugf("[UDP] 匹配代理: %s -> %s", domain, upstreamProxy)
			targetEndpoint := fmt.Sprintf("%s:%d", strings.TrimSuffix(domain, "."), fakeIP.Port)
			upstreamConn, err = client.Dial("udp", targetEndpoint)
			if err != nil && strings.Contains(err.Error(), "address type not supported") {
				zap.S().Warnf("[UDP] 上遊不支持域名解析，嘗試本地解析後發送 IP: %s", domain)
				ips, _ := defaultResolver.LookupIP(domain)
				if len(ips) > 0 {
					targetEndpointIP := fmt.Sprintf("%s:%d", ips[0].String(), fakeIP.Port)
					upstreamConn, err = client.Dial("udp", targetEndpointIP)
				}
			}

			// 錯誤攔截，防止 upstreamConn 為 nil 導致 Panic
			if err != nil || upstreamConn == nil {
				zap.S().Errorf("[UDP] SOCKS5 撥號失敗 %s: %v", domain, err)
				return
			}
		} else {
			zap.S().Debugf("[UDP] 匹配直連: %s", domain)
			ips, resolveErr := defaultResolver.LookupIP(domain)
			if resolveErr != nil || len(ips) == 0 {
				zap.S().Warnf("[UDP] 直連解析 %s 失敗: %v", domain, resolveErr)
				return
			}

			realTarget := fmt.Sprintf("%s:%d", ips[0].String(), fakeIP.Port)
			upstreamConn, err = net.DialTimeout("udp", realTarget, 5*time.Second)
			if err != nil {
				zap.S().Warnf("[UDP] 直連撥號 %s 失敗: %v", realTarget, err)
				return
			}
		}

		sess = &UDPSession{UpstreamConn: upstreamConn, LastActive: time.Now()}
		sessionMu.Lock()
		udpSessions[sessionKey] = sess
		sessionMu.Unlock()

		go listenFromUpstream(sess, clientAddr, fakeIP, sessionKey)
	}

	sess.LastActive = time.Now()
	_, err := sess.UpstreamConn.Write(payload)
	if err != nil {
		zap.S().Debugf("[UDP] 寫入上遊失敗: %v", err)
		sessionMu.Lock()
		delete(udpSessions, sessionKey)
		sessionMu.Unlock()
		sess.UpstreamConn.Close()
	}
}

func listenFromUpstream(sess *UDPSession, clientAddr *net.UDPAddr, fakeIP *net.UDPAddr, sessionKey string) {
	defer func() {
		sessionMu.Lock()
		delete(udpSessions, sessionKey)
		sessionMu.Unlock()
		sess.UpstreamConn.Close()
	}()

	buf := make([]byte, 65536)
	for {
		n, err := sess.UpstreamConn.Read(buf)
		if err != nil {
			return
		}
		sess.LastActive = time.Now()
		sendBackToClient(buf[:n], clientAddr, fakeIP)
	}
}

func sendBackToClient(data []byte, clientAddr *net.UDPAddr, fakeIP *net.UDPAddr) {
	dialer := net.Dialer{
		LocalAddr: fakeIP,
		Control: func(network, address string, c syscall.RawConn) error {
			var err error
			c.Control(func(fd uintptr) {
				err = unix.SetsockoptInt(int(fd), unix.SOL_IPV6, unix.IPV6_TRANSPARENT, 1)
			})
			return err
		},
	}
	conn, _ := dialer.Dial("udp6", clientAddr.String())
	if conn != nil {
		conn.Write(data)
		conn.Close()
	}
}

func startUDPSweeper(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			zap.S().Infof("[UDP] 停止會話垃圾回收器")
			return
		case <-ticker.C:
			now := time.Now()
			sessionMu.Lock()
			for key, sess := range udpSessions {
				if now.Sub(sess.LastActive) > 3*time.Minute {
					sess.UpstreamConn.Close()
					delete(udpSessions, key)
				}
			}
			sessionMu.Unlock()
		}
	}
}
