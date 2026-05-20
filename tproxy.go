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

type udpKey struct {
	clientIP   [16]byte
	fakeIP     [16]byte
	clientPort uint16
	fakePort   uint16
}

type UDPSession struct {
	UpstreamConn     net.Conn
	ClientReturnConn net.Conn // 驻缓存返回客户端的物理 Socket，拒绝单包重拨
	LastActive       time.Time
}

var (
	socksClientCache = make(map[string]*socks5.Client)
	udpClientMu      sync.RWMutex

	// TCP 转发用的 32KB 缓冲区池 (io.Copy 默认大小)
	tcpBufferPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 32*1024)
		},
	}

	// UDP Payload 用的 64KB 缓冲区池 (最大 UDP 包大小)
	udpBufferPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 65536)
		},
	}

	udpSessions = make(map[udpKey]*UDPSession)
	sessionMu   sync.RWMutex
)

// 生成零堆分配分配的 udpKey
func newUDPKey(clientAddr, fakeIP *net.UDPAddr) udpKey {
	var k udpKey
	copy(k.clientIP[:], clientAddr.IP.To16())
	copy(k.fakeIP[:], fakeIP.IP.To16())
	k.clientPort = uint16(clientAddr.Port)
	k.fakePort = uint16(fakeIP.Port)
	return k
}

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
	c, err := socks5.NewClient(addr, user, pass, 2*60, 2*60)
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

		// 重试 (应对 Tor 的首次寻径失败)
		maxRetries := 5
		for i := 0; i < maxRetries; i++ {
			targetConn, err = client.Dial("tcp", targetEndpoint)
			if err == nil {
				// 拨号成功，跳出循环
				break
			}

			zap.S().Warnf("[TCP] SOCKS5 撥號失敗 (%d/%d): %s, 錯誤: %v", i+1, maxRetries, domain, err)

			// 处理不支持域名的情况 (IP 降级)
			if strings.Contains(err.Error(), "address type not supported") {
				zap.S().Warnf("[TCP] 上遊不支持域名解析，嘗試本地解析後發送 IP: %s", domain)
				ips, _ := defaultResolver.LookupIP(domain)
				if len(ips) > 0 {
					targetConn, err = client.Dial("tcp", fmt.Sprintf("%s:%d", ips[0].String(), targetAddr.Port))
					if err == nil {
						break
					}
				}
				// 降级 IP 也失败，说明彻底不通，不再重试
				break
			}

			// 如果还没到最大重试次数，等待 3 秒后重试 (给 Tor 建立回路的时间)
			if i < maxRetries-1 {
				time.Sleep(3 * time.Second)
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

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		buf := tcpBufferPool.Get().([]byte)
		defer tcpBufferPool.Put(buf)

		io.CopyBuffer(targetConn, clientConn, buf)
		if tc, ok := targetConn.(*net.TCPConn); ok {
			tc.CloseWrite() // 结合了优化点 1 的优雅半关闭
		}
	}()

	go func() {
		defer wg.Done()
		buf := tcpBufferPool.Get().([]byte)
		defer tcpBufferPool.Put(buf)

		io.CopyBuffer(clientConn, targetConn, buf)
		if tc, ok := clientConn.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()

	wg.Wait()
}

func startUDPTProxy(ctx context.Context, addr string) {
	lc := net.ListenConfig{Control: setTransparentSocket}
	packetConn, err := lc.ListenPacket(context.Background(), "udp6", addr)
	if err != nil {
		zap.S().Fatalf("UDP TProxy 失敗: %v", err)
	}
	zap.S().Infof("UDP TProxy 啟端於 %s", addr)
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
			continue
		}
		fakeIPAddr, err := parseIPv6OriginalDst(oob[:oobn])
		if err != nil {
			continue
		}

		payload := udpBufferPool.Get().([]byte)[:n]
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
	sessionKey := newUDPKey(clientAddr, fakeIP) // 高速查表，零分配

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
		returnConn, err := dialer.Dial("udp6", clientAddr.String())
		if err != nil {
			zap.S().Errorf("[UDP] 无法建立回传 Socket %v", err)
			upstreamConn.Close()
			return
		}

		sess = &UDPSession{
			UpstreamConn:     upstreamConn,
			ClientReturnConn: returnConn, // 锁入会话中
			LastActive:       time.Now(),
		}

		sessionMu.Lock()
		udpSessions[sessionKey] = sess
		sessionMu.Unlock()

		go listenFromUpstream(sess, sessionKey)
	}

	sess.LastActive = time.Now()
	_, err := sess.UpstreamConn.Write(payload)
	if err != nil {
		zap.S().Debugf("[UDP] 寫入上遊失敗: %v", err)
		sessionMu.Lock()
		delete(udpSessions, sessionKey)
		sessionMu.Unlock()
		sess.UpstreamConn.Close()
		sess.ClientReturnConn.Close()
	}
}

func listenFromUpstream(sess *UDPSession, sessionKey udpKey) {
	defer func() {
		sessionMu.Lock()
		delete(udpSessions, sessionKey)
		sessionMu.Unlock()
		sess.UpstreamConn.Close()
		sess.ClientReturnConn.Close() // 同步销毁两端 Socket 物理层
	}()

	buf := udpBufferPool.Get().([]byte)
	defer udpBufferPool.Put(buf)

	for {
		sess.UpstreamConn.SetReadDeadline(time.Now().Add(3 * time.Minute))
		n, err := sess.UpstreamConn.Read(buf)
		if err != nil {
			return
		}

		sess.LastActive = time.Now()
		sess.ClientReturnConn.Write(buf[:n])
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
					sess.ClientReturnConn.Close() // 全面安全释放
					delete(udpSessions, key)
				}
			}
			sessionMu.Unlock()
		}
	}
}
