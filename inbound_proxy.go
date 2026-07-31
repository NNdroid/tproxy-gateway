package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/txthinking/socks5"
	"go.uber.org/zap"
)

func startInboundProxies(ctx context.Context, sCfg ServerConfig) {
	if sCfg.SocksAddr != "" {
		go startSocksInbound(ctx, sCfg.SocksAddr)
	}
	if sCfg.HTTPAddr != "" {
		go startHTTPInbound(ctx, sCfg.HTTPAddr)
	}
}

func startSocksInbound(ctx context.Context, addr string) {
	server, err := socks5.NewClassicServer(addr, "0.0.0.0", "", "", 60, 60)
	if err != nil {
		zap.S().Errorf("[Inbound] Failed to create SOCKS5 inbound: %v", err)
		return
	}

	zap.S().Infof("🔌 SOCKS5 inbound proxy started -> socks5://%s", addr)

	go func() {
		if err := server.ListenAndServe(nil); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			zap.S().Debugf("[Inbound] SOCKS5 inbound proxy stopped: %v", err)
		}
	}()
}

func startHTTPInbound(ctx context.Context, addr string) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		zap.S().Errorf("[Inbound] Failed to create HTTP inbound listener: %v", err)
		return
	}
	zap.S().Infof("🔌 HTTP inbound proxy started -> http://%s", addr)

	go func() {
		<-ctx.Done()
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
		go handleHTTPInboundConn(conn)
	}
}

func handleHTTPInboundConn(clientConn net.Conn) {
	defer clientConn.Close()

	reader := bufio.NewReader(clientConn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}

	host := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		if req.Method == "CONNECT" {
			port = "443"
		} else {
			port = "80"
		}
	}
	targetAddrStr := fmt.Sprintf("%s:%s", host, port)

	upstreamProxy, rewrites := globalRuleEngine.Match(host, netip.Addr{})
	upstreamProxy = globalChecker.SelectProxy(upstreamProxy)

	isDirect := upstreamProxy == "" || strings.ToUpper(upstreamProxy) == "DIRECT"
	isReject := strings.ToUpper(upstreamProxy) == "REJECT"

	if isReject {
		return
	}

	var targetConn net.Conn

	if !isDirect {
		client, cErr := getSocksClient(upstreamProxy)
		if cErr != nil {
			return
		}
		targetConn, err = client.Dial("tcp", targetAddrStr)
	} else {
		targetConn, err = net.DialTimeout("tcp", targetAddrStr, 5*time.Second)
	}

	if err != nil || targetConn == nil {
		return
	}
	defer targetConn.Close()

	if req.Method == "CONNECT" {
		clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	} else {
		if len(rewrites) > 0 {
			for k, v := range rewrites {
				req.Header.Set(k, v)
			}
		}
		req.Write(targetConn)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(targetConn, reader)
		if cw, ok := targetConn.(closeWriter); ok {
			cw.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		io.Copy(clientConn, targetConn)
		if cw, ok := clientConn.(closeWriter); ok {
			cw.CloseWrite()
		}
	}()
	wg.Wait()
}
