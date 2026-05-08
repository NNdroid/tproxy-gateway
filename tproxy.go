package main

import (
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
	"golang.org/x/net/proxy"
	"golang.org/x/sys/unix"
)

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
		zap.S().Fatalf("TCP TProxy 失败: %v", err)
	}
	zap.S().Infof("TCP TProxy 启动于 %s", addr)

	go func() {
		<-ctx.Done()
		zap.S().Infof("[TCP] 正在关闭 TProxy 监听器...")
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

	targetEndpoint := fmt.Sprintf("%s:%d", domain, targetAddr.Port)

	upstreamProxy, matched := router.Match(domain)
	if !matched {
		upstreamProxy = cfg.Routing.DefaultUpstream
	}

	isDirect := upstreamProxy == "" || strings.ToUpper(upstreamProxy) == "DIRECT"
	isReject := strings.ToUpper(upstreamProxy) == "REJECT"

	if isReject {
		zap.S().Debugf("[TCP] 拦截请求: %s (命中 REJECT)", domain)
		return
	}

	var targetConn net.Conn
	var err error

	if !isDirect {
		zap.S().Debugf("[TCP] 匹配代理: %s -> %s", domain, upstreamProxy)
		dialer, _ := proxy.SOCKS5("tcp", upstreamProxy, nil, proxy.Direct)
		targetConn, err = dialer.Dial("tcp", targetEndpoint)
	} else {
		zap.S().Debugf("[TCP] 匹配直连: %s", domain)
		ips, resolveErr := defaultResolver.LookupIP(domain)
		if resolveErr != nil || len(ips) == 0 {
			zap.S().Warnf("[TCP] 直连解析 %s 失败: %v", domain, resolveErr)
			return
		}

		for _, ip := range ips {
			addr := fmt.Sprintf("%s:%d", ip.String(), targetAddr.Port)
			targetConn, err = net.DialTimeout("tcp", addr, 3*time.Second)
			if err == nil {
				zap.S().Debugf("[TCP] 直连拨号成功: %s -> %s", domain, addr)
				break
			}
		}

		if targetConn == nil {
			zap.S().Warnf("[TCP] 直连 %s 所有 IP 均失败", domain)
			return
		}
	}

	if err != nil {
		zap.S().Errorf("[TCP] 连接失败 %s: %v", domain, err)
		return
	}
	defer targetConn.Close()

	go io.Copy(targetConn, clientConn)
	io.Copy(clientConn, targetConn)
}

// ========== UDP 部分 ==========

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
		zap.S().Fatalf("UDP TProxy 失败: %v", err)
	}
	zap.S().Infof("UDP TProxy 启动于 %s", addr)
	udpConn := packetConn.(*net.UDPConn)

	go func() {
		<-ctx.Done()
		zap.S().Infof("[UDP] 正在关闭 TProxy 监听器...")
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

		upstreamProxy, matched := router.Match(domain)
		if !matched {
			upstreamProxy = cfg.Routing.DefaultUpstream
		}

		isDirect := upstreamProxy == "" || strings.ToUpper(upstreamProxy) == "DIRECT"
		isReject := strings.ToUpper(upstreamProxy) == "REJECT"

		if isReject {
			zap.S().Debugf("[UDP] 拦截请求: %s (命中 REJECT)", domain)
			return
		}

		var upstreamConn net.Conn
		var err error

		if !isDirect {
			zap.S().Debugf("[UDP] 匹配代理: %s -> %s", domain, upstreamProxy)
			client, err := socks5.NewClient(upstreamProxy, "", "", 10, 10)
			if err != nil {
				zap.S().Errorf("[UDP] SOCKS5 客户端创建失败: %v", err)
				return
			}
			upstreamConn, err = client.Dial("udp", fmt.Sprintf("%s:%d", domain, fakeIP.Port))
			if err != nil {
				zap.S().Errorf("[UDP] SOCKS5 拨号失败: %v", err)
				return
			}
		} else {
			zap.S().Debugf("[UDP] 匹配直连: %s", domain)
			ips, resolveErr := defaultResolver.LookupIP(domain)
			if resolveErr != nil || len(ips) == 0 {
				zap.S().Warnf("[UDP] 直连解析 %s 失败: %v", domain, resolveErr)
				return
			}

			realTarget := fmt.Sprintf("%s:%d", ips[0].String(), fakeIP.Port)
			upstreamConn, err = net.DialTimeout("udp", realTarget, 5*time.Second)
			if err != nil {
				zap.S().Warnf("[UDP] 直连拨号 %s 失败: %v", realTarget, err)
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
		zap.S().Debugf("[UDP] 写入上游失败: %v", err)
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
			zap.S().Infof("[UDP] 停止会话垃圾回收器")
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