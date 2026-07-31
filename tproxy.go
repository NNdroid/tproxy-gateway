package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/txthinking/socks5"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

type closeWriter interface {
	CloseWrite() error
}

type udpKey struct {
	clientIP   netip.Addr
	fakeIP     netip.Addr
	clientPort uint16
	fakePort   uint16
}

type UDPSession struct {
	UpstreamConn     net.Conn
	ClientReturnConn net.Conn
	LastActive       time.Time
}

type ConnInfo struct {
	ID        string    `json:"id"`
	ClientIP  string    `json:"client_ip"`
	Target    string    `json:"target"`
	Domain    string    `json:"domain"`
	Upstream  string    `json:"upstream"`
	StartTime time.Time `json:"start_time"`
	conn      net.Conn
}

type ConnTrackerStore struct {
	mu    sync.RWMutex
	conns map[string]*ConnInfo
	seq   uint64
}

var (
	GlobalConnTracker = &ConnTrackerStore{
		conns: make(map[string]*ConnInfo),
	}

	socksClientCache = make(map[string]*socks5.Client)
	udpClientMu      sync.RWMutex

	tcpBufferPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 32*1024)
		},
	}

	udpBufferPool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 65536)
		},
	}

	udpSessions = make(map[udpKey]*UDPSession)
	sessionMu   sync.RWMutex
)

func (cts *ConnTrackerStore) Add(clientIP, target, domain, upstream string, conn net.Conn) string {
	cts.mu.Lock()
	defer cts.mu.Unlock()
	cts.seq++
	id := fmt.Sprintf("conn-%d", cts.seq)
	info := &ConnInfo{
		ID:        id,
		ClientIP:  clientIP,
		Target:    target,
		Domain:    domain,
		Upstream:  upstream,
		StartTime: time.Now(),
		conn:      conn,
	}
	cts.conns[id] = info
	return id
}

func (cts *ConnTrackerStore) Remove(id string) {
	cts.mu.Lock()
	defer cts.mu.Unlock()
	delete(cts.conns, id)
}

func (cts *ConnTrackerStore) Kill(id string) bool {
	cts.mu.Lock()
	info, exists := cts.conns[id]
	if exists {
		delete(cts.conns, id)
	}
	cts.mu.Unlock()

	if exists && info.conn != nil {
		_ = info.conn.Close()
		return true
	}
	return false
}

func (cts *ConnTrackerStore) List() []map[string]interface{} {
	cts.mu.RLock()
	defer cts.mu.RUnlock()

	now := time.Now()
	list := make([]map[string]interface{}, 0, len(cts.conns))
	for id, info := range cts.conns {
		duration := now.Sub(info.StartTime).Truncate(time.Second).String()
		list = append(list, map[string]interface{}{
			"id":         id,
			"client_ip":  info.ClientIP,
			"target":     info.Target,
			"domain":     info.Domain,
			"upstream":   info.Upstream,
			"start_time": info.StartTime.Format("15:04:05"),
			"duration":   duration,
		})
	}
	return list
}

func tuneTCPSocket(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}
}

func newUDPKey(clientAddr, fakeIP *net.UDPAddr) (udpKey, bool) {
	cAddr, ok1 := netip.AddrFromSlice(clientAddr.IP)
	fAddr, ok2 := netip.AddrFromSlice(fakeIP.IP)
	if !ok1 || !ok2 {
		return udpKey{}, false
	}
	return udpKey{
		clientIP:   cAddr.Unmap(),
		fakeIP:     fAddr.Unmap(),
		clientPort: uint16(clientAddr.Port),
		fakePort:   uint16(fakeIP.Port),
	}, true
}

func getSocksClient(proxyAddr string) (*socks5.Client, error) {
	cleanAddr := strings.TrimPrefix(proxyAddr, "socks5://")
	udpClientMu.RLock()
	c, exists := socksClientCache[cleanAddr]
	udpClientMu.RUnlock()
	if exists {
		return c, nil
	}

	udpClientMu.Lock()
	defer udpClientMu.Unlock()

	user, pass, addr := parseSocksAddr(cleanAddr)
	c, err := socks5.NewClient(addr, user, pass, 2*60, 2*60)
	if err == nil {
		socksClientCache[cleanAddr] = c
	}
	return c, err
}

func dialProxyConn(proxyAddr string, network string, targetEndpoint string, timeout time.Duration) (net.Conn, error) {
	if strings.HasPrefix(proxyAddr, "http://") || strings.HasPrefix(proxyAddr, "https://") {
		zap.S().Debugf("[Outbound] Handshaking HTTP CONNECT proxy: %s -> %s", proxyAddr, targetEndpoint)
		u, err := url.Parse(proxyAddr)
		if err != nil {
			return nil, err
		}
		host := u.Host
		if !strings.Contains(host, ":") {
			if u.Scheme == "https" {
				host += ":443"
			} else {
				host += ":80"
			}
		}

		conn, err := net.DialTimeout("tcp", host, timeout)
		if err != nil {
			return nil, err
		}
		tuneTCPSocket(conn)

		req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", targetEndpoint, targetEndpoint)
		if u.User != nil {
			auth := base64.StdEncoding.EncodeToString([]byte(u.User.String()))
			req += fmt.Sprintf("Proxy-Authorization: Basic %s\r\n", auth)
		}
		req += "\r\n"

		conn.SetDeadline(time.Now().Add(timeout))
		if _, err := conn.Write([]byte(req)); err != nil {
			conn.Close()
			return nil, err
		}

		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			conn.Close()
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, fmt.Errorf("http proxy CONNECT failed: %s", resp.Status)
		}
		conn.SetDeadline(time.Time{})
		zap.S().Debugf("[Outbound] HTTP CONNECT tunnel established: %s -> %s", proxyAddr, targetEndpoint)
		return conn, nil
	}

	zap.S().Debugf("[Outbound] Handshaking SOCKS5 proxy: %s -> %s (%s)", proxyAddr, targetEndpoint, network)
	client, err := getSocksClient(proxyAddr)
	if err != nil {
		return nil, err
	}
	return client.Dial(network, targetEndpoint)
}

func setTransparentSocket(network, address string, c syscall.RawConn) error {
	var err error
	c.Control(func(fd uintptr) {
		unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
		unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_TRANSPARENT, 1)
		unix.SetsockoptInt(int(fd), unix.SOL_IPV6, unix.IPV6_TRANSPARENT, 1)
		unix.SetsockoptInt(int(fd), unix.SOL_IPV6, unix.IPV6_V6ONLY, 0)
		if strings.HasPrefix(network, "udp") {
			unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_RECVORIGDSTADDR, 1)
			unix.SetsockoptInt(int(fd), unix.SOL_IPV6, unix.IPV6_RECVORIGDSTADDR, 1)
		}
	})
	return err
}

func startTCPTProxy(ctx context.Context, addr string) {
	lc := net.ListenConfig{Control: setTransparentSocket}
	listener, err := lc.Listen(context.Background(), "tcp6", addr)
	if err != nil {
		zap.S().Fatalf("TCP TProxy failed: %v", err)
	}
	zap.S().Infof("TCP TProxy listening on %s", addr)

	go func() {
		<-ctx.Done()
		zap.S().Infof("[TCP] Shutting down TCP TProxy listener...")
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
		tuneTCPSocket(conn)
		go handleTCP(conn)
	}
}

func handleTCP(clientConn net.Conn) {
	Metrics.ActiveTCPConnections.Add(1)
	defer Metrics.ActiveTCPConnections.Add(-1)

	targetAddr := clientConn.LocalAddr().(*net.TCPAddr)
	targetIP, _ := netip.AddrFromSlice(targetAddr.IP)
	domain, _ := pool.LookUp(targetAddr.IP.String())

	if GlobalAnalytics != nil {
		GlobalAnalytics.RecordTCPConn(domain)
	}

	if cfg.UI.Enabled && cfg.UI.Domain != "" {
		uiMode := strings.ToLower(strings.TrimSpace(cfg.UI.Mode))
		if uiMode == "" || uiMode == "domain" || uiMode == "both" {
			cleanUIDomain := strings.TrimSuffix(cfg.UI.Domain, ".")
			cleanDomain := strings.TrimSuffix(domain, ".")
			if strings.EqualFold(cleanDomain, cleanUIDomain) {
				zap.S().Debugf("[WebUI] Intercepted Virtual Domain request: %s -> Serving WebUI", domain)
				handleVirtualDomainConn(clientConn)
				return
			}
		}
	}

	defer clientConn.Close()

	upstreamProxy, rewrites := globalRuleEngine.Match(domain, targetIP)
	upstreamProxy = globalChecker.SelectProxy(upstreamProxy)

	isDirect := upstreamProxy == "" || strings.ToUpper(upstreamProxy) == "DIRECT"
	isReject := strings.ToUpper(upstreamProxy) == "REJECT"

	targetEndpoint := domain
	if targetEndpoint == "" {
		targetEndpoint = targetAddr.IP.String()
	}
	targetEndpoint = fmt.Sprintf("%s:%d", strings.TrimSuffix(targetEndpoint, "."), targetAddr.Port)

	zap.S().Debugf("[TCP] New connection from %s -> %s (FakeIP: %s, Domain: '%s', Upstream: '%s')",
		clientConn.RemoteAddr(), targetEndpoint, targetIP, domain, upstreamProxy)

	if isReject {
		zap.S().Debugf("[TCP] Request rejected: %s / %s (matched REJECT rule)", domain, targetIP)
		return
	}

	var targetConn net.Conn
	var err error

	if !isDirect {
		zap.S().Debugf("[TCP] Matched proxy node [%s]: %s -> %s", upstreamProxy, domain, targetEndpoint)

		backoffs := []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 1 * time.Second}
		for i := 0; i <= len(backoffs); i++ {
			zap.S().Debugf("[TCP] Attempting connection via proxy [%s] to %s (Try %d/%d)...", upstreamProxy, targetEndpoint, i+1, len(backoffs)+1)
			targetConn, err = dialProxyConn(upstreamProxy, "tcp", targetEndpoint, 30*time.Second)
			if err == nil {
				zap.S().Debugf("[TCP] Successfully connected to %s via proxy [%s]", targetEndpoint, upstreamProxy)
				break
			}

			zap.S().Warnf("[TCP] Proxy dial failed (%d/%d): %s, error: %v", i+1, len(backoffs)+1, targetEndpoint, err)

			if strings.Contains(err.Error(), "address type not supported") && domain != "" {
				zap.S().Warnf("[TCP] Upstream unsupported domain resolution, trying local DNS resolution for IP: %s", domain)
				ips, _ := defaultResolver.LookupIP(domain)
				if len(ips) > 0 {
					targetConn, err = dialProxyConn(upstreamProxy, "tcp", fmt.Sprintf("%s:%d", ips[0].String(), targetAddr.Port), 3*time.Second)
					if err == nil {
						zap.S().Debugf("[TCP] Successfully connected to resolved IP %s via proxy [%s]", ips[0].String(), upstreamProxy)
						break
					}
				}
				break
			}

			if i < len(backoffs) {
				time.Sleep(backoffs[i])
			}
		}
	} else {
		zap.S().Debugf("[TCP] Matched DIRECT: %s / %s", domain, targetEndpoint)
		dialer := net.Dialer{Timeout: 3 * time.Second}
		if domain != "" {
			ips, resolveErr := defaultResolver.LookupIP(domain)
			if resolveErr == nil && len(ips) > 0 {
				zap.S().Debugf("[TCP] Direct resolving '%s' -> %v", domain, ips)
				for _, ip := range ips {
					addr := fmt.Sprintf("%s:%d", ip.String(), targetAddr.Port)
					targetConn, err = dialer.Dial("tcp", addr)
					if err == nil {
						zap.S().Debugf("[TCP] Directly connected to %s (%s)", domain, addr)
						break
					}
				}
			}
		}
		if targetConn == nil {
			targetConn, err = dialer.Dial("tcp", fmt.Sprintf("%s:%d", targetAddr.IP.String(), targetAddr.Port))
			if err == nil {
				zap.S().Debugf("[TCP] Directly connected to IP %s:%d", targetAddr.IP.String(), targetAddr.Port)
			}
		}
	}

	if err != nil || targetConn == nil {
		zap.S().Errorf("[TCP] Failed to connect to upstream %s: %v", targetEndpoint, err)
		return
	}
	defer targetConn.Close()
	tuneTCPSocket(targetConn)

	connID := GlobalConnTracker.Add(clientConn.RemoteAddr().String(), targetEndpoint, domain, upstreamProxy, clientConn)
	defer GlobalConnTracker.Remove(connID)

	if targetAddr.Port == 80 && len(rewrites) > 0 {
		reader := bufio.NewReader(clientConn)
		newHeader, err := rewriteHTTPHeader(reader, rewrites)
		if err == nil {
			zap.S().Debugf("[TCP] Applied HTTP Header Rewrites for %s: %v", targetEndpoint, rewrites)
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
		if cw, ok := targetConn.(closeWriter); ok {
			cw.CloseWrite()
		}
	}()

	go func() {
		defer wg.Done()
		buf := tcpBufferPool.Get().([]byte)
		defer tcpBufferPool.Put(buf)

		io.CopyBuffer(clientConn, targetConn, buf)
		if cw, ok := clientConn.(closeWriter); ok {
			cw.CloseWrite()
		}
	}()

	wg.Wait()
	zap.S().Debugf("[TCP] Connection closed: %s <-> %s", clientConn.RemoteAddr(), targetEndpoint)
}

func startUDPTProxy(ctx context.Context, addr string) {
	lc := net.ListenConfig{Control: setTransparentSocket}
	packetConn, err := lc.ListenPacket(context.Background(), "udp6", addr)
	if err != nil {
		zap.S().Fatalf("UDP TProxy failed: %v", err)
	}
	zap.S().Infof("UDP TProxy listening on %s", addr)
	udpConn := packetConn.(*net.UDPConn)

	go func() {
		<-ctx.Done()
		zap.S().Infof("[UDP] Shutting down UDP TProxy listener...")
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

		key, ok := newUDPKey(clientAddr, fakeIPAddr)
		if ok {
			sessionMu.RLock()
			sess, exists := udpSessions[key]
			if exists {
				sess.LastActive = time.Now()
				_, _ = sess.UpstreamConn.Write(buf[:n])
				sessionMu.RUnlock()
				zap.S().Debugf("[UDP] Fast-path packet forwarded: %s -> %s (%d bytes)", clientAddr, fakeIPAddr, n)
				continue
			}
			sessionMu.RUnlock()
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
		if (msg.Header.Level == unix.SOL_IPV6 && msg.Header.Type == unix.IPV6_RECVORIGDSTADDR) ||
			(msg.Header.Level == unix.SOL_IP && msg.Header.Type == unix.IP_RECVORIGDSTADDR) {
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
	defer udpBufferPool.Put(payload)

	sessionKey, ok := newUDPKey(clientAddr, fakeIP)
	if !ok {
		return
	}

	sessionMu.RLock()
	sess, exists := udpSessions[sessionKey]
	sessionMu.RUnlock()

	if !exists {
		domain, _ := pool.LookUp(fakeIP.IP.String())
		targetIP, _ := netip.AddrFromSlice(fakeIP.IP)

		if GlobalAnalytics != nil {
			GlobalAnalytics.RecordUDPConn(domain)
		}

		upstreamProxy, _ := globalRuleEngine.Match(domain, targetIP)
		upstreamProxy = globalChecker.SelectProxy(upstreamProxy)

		zap.S().Debugf("[UDP] New session request: %s -> %s (Domain: '%s', Upstream: '%s')", clientAddr, fakeIP, domain, upstreamProxy)

		isDirect := upstreamProxy == "" || strings.ToUpper(upstreamProxy) == "DIRECT"
		isReject := strings.ToUpper(upstreamProxy) == "REJECT"

		if isReject {
			zap.S().Debugf("[UDP] Request rejected: %s (matched REJECT rule)", domain)
			return
		}

		var upstreamConn net.Conn
		var err error

		if !isDirect {
			targetEndpoint := domain
			if targetEndpoint == "" {
				targetEndpoint = fakeIP.IP.String()
			}
			targetEndpoint = fmt.Sprintf("%s:%d", strings.TrimSuffix(targetEndpoint, "."), fakeIP.Port)
			zap.S().Debugf("[UDP] Dialing UDP proxy [%s] for %s...", upstreamProxy, targetEndpoint)
			upstreamConn, err = dialProxyConn(upstreamProxy, "udp", targetEndpoint, 3*time.Second)
			if err != nil && strings.Contains(err.Error(), "address type not supported") && domain != "" {
				ips, _ := defaultResolver.LookupIP(domain)
				if len(ips) > 0 {
					targetEndpointIP := fmt.Sprintf("%s:%d", ips[0].String(), fakeIP.Port)
					upstreamConn, err = dialProxyConn(upstreamProxy, "udp", targetEndpointIP, 3*time.Second)
				}
			}

			if err != nil || upstreamConn == nil {
				zap.S().Errorf("[UDP] Proxy dial failed %s: %v", targetEndpoint, err)
				return
			}
		} else {
			realTarget := fmt.Sprintf("%s:%d", fakeIP.IP.String(), fakeIP.Port)
			if domain != "" {
				ips, resolveErr := defaultResolver.LookupIP(domain)
				if resolveErr == nil && len(ips) > 0 {
					realTarget = fmt.Sprintf("%s:%d", ips[0].String(), fakeIP.Port)
				}
			}
			zap.S().Debugf("[UDP] Direct dialing UDP %s...", realTarget)
			upstreamConn, err = net.DialTimeout("udp", realTarget, 5*time.Second)
			if err != nil {
				zap.S().Warnf("[UDP] Direct dial %s failed: %v", realTarget, err)
				return
			}
		}

		dialer := net.Dialer{
			LocalAddr: fakeIP,
			Control: func(network, address string, c syscall.RawConn) error {
				var err error
				c.Control(func(fd uintptr) {
					unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
					unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
					unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_TRANSPARENT, 1)
					unix.SetsockoptInt(int(fd), unix.SOL_IPV6, unix.IPV6_TRANSPARENT, 1)
				})
				return err
			},
		}
		returnConn, err := dialer.Dial("udp6", clientAddr.String())
		if err != nil {
			zap.S().Errorf("[UDP] Failed to create return socket: %v", err)
			upstreamConn.Close()
			return
		}

		sess = &UDPSession{
			UpstreamConn:     upstreamConn,
			ClientReturnConn: returnConn,
			LastActive:       time.Now(),
		}

		sessionMu.Lock()
		udpSessions[sessionKey] = sess
		Metrics.ActiveUDPSessions.Store(int64(len(udpSessions)))
		sessionMu.Unlock()

		zap.S().Debugf("[UDP] Session established: %s <-> %s", clientAddr, fakeIP)
		go listenFromUpstream(sess, sessionKey)
	}

	sess.LastActive = time.Now()
	_, err := sess.UpstreamConn.Write(payload)
	if err != nil {
		zap.S().Debugf("[UDP] Failed to write to upstream: %v", err)
		sessionMu.Lock()
		delete(udpSessions, sessionKey)
		Metrics.ActiveUDPSessions.Store(int64(len(udpSessions)))
		sessionMu.Unlock()
		sess.UpstreamConn.Close()
		sess.ClientReturnConn.Close()
	}
}

func listenFromUpstream(sess *UDPSession, sessionKey udpKey) {
	defer func() {
		sessionMu.Lock()
		delete(udpSessions, sessionKey)
		Metrics.ActiveUDPSessions.Store(int64(len(udpSessions)))
		sessionMu.Unlock()
		sess.UpstreamConn.Close()
		sess.ClientReturnConn.Close()
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
			zap.S().Infof("[UDP] Stopping session garbage collector")
			return
		case <-ticker.C:
			now := time.Now()
			sessionMu.Lock()
			count := 0
			for key, sess := range udpSessions {
				if now.Sub(sess.LastActive) > 3*time.Minute {
					sess.UpstreamConn.Close()
					sess.ClientReturnConn.Close()
					delete(udpSessions, key)
					count++
				}
			}
			if count > 0 {
				zap.S().Debugf("[UDP] Sweeper cleaned up %d inactive UDP sessions", count)
			}
			Metrics.ActiveUDPSessions.Store(int64(len(udpSessions)))
			sessionMu.Unlock()
		}
	}
}
