package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

//go:embed webui/*
var webuiFS embed.FS

var (
	globalWebUIMux  *http.ServeMux
	tlsConfigOnce   sync.Once
	cachedTLSConfig *tls.Config
)

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func newBufferedConn(c net.Conn) *bufferedConn {
	return &bufferedConn{
		Conn: c,
		r:    bufio.NewReader(c),
	}
}

func (b *bufferedConn) PeekByte() (byte, error) {
	bytes, err := b.r.Peek(1)
	if err != nil {
		return 0, err
	}
	return bytes[0], nil
}

func (b *bufferedConn) Read(p []byte) (int, error) {
	return b.r.Read(p)
}

type singleConnListener struct {
	conn net.Conn
	done chan struct{}
	once sync.Once
}

func newSingleConnListener(c net.Conn) *singleConnListener {
	return &singleConnListener{
		conn: c,
		done: make(chan struct{}),
	}
}

func (s *singleConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	s.once.Do(func() {
		c = s.conn
	})
	if c != nil {
		return c, nil
	}
	<-s.done
	return nil, net.ErrClosed
}

func (s *singleConnListener) Close() error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return nil
}

func (s *singleConnListener) Addr() net.Addr {
	return s.conn.LocalAddr()
}

func getVirtualDomainTLSConfig(domain string) *tls.Config {
	tlsConfigOnce.Do(func() {
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			zap.S().Errorf("[Dashboard] Failed to generate TLS private key: %v", err)
			return
		}

		cleanDomain := strings.TrimSuffix(domain, ".")
		if cleanDomain == "" {
			cleanDomain = "dashboard.gateway"
		}

		template := x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()),
			Subject: pkix.Name{
				Organization: []string{"TProxy Gateway Virtual WebUI"},
				CommonName:   cleanDomain,
			},
			NotBefore:             time.Now().Add(-1 * time.Hour),
			NotAfter:              time.Now().Add(365 * 24 * time.Hour),
			KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			BasicConstraintsValid: true,
			DNSNames:              []string{cleanDomain, "*." + cleanDomain, "localhost"},
		}

		derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
		if err != nil {
			zap.S().Errorf("[Dashboard] Failed to create TLS certificate: %v", err)
			return
		}

		cert := tls.Certificate{
			Certificate: [][]byte{derBytes},
			PrivateKey:  priv,
		}

		cachedTLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
		}
		zap.S().Infof("🔒 Generated self-signed TLS certificate for Virtual Domain WebUI -> https://%s/", cleanDomain)
	})
	return cachedTLSConfig
}

func initDashboardRoutes(cfg UIConfig, defaultPortAddr string) *http.ServeMux {
	mux := http.NewServeMux()

	if cfg.Enabled {
		subFS, err := fs.Sub(webuiFS, "webui")
		if err != nil {
			zap.S().Errorf("[Dashboard] Failed to mount WebUI static assets: %v", err)
		} else {
			uiHandler := http.FileServer(http.FS(subFS))
			mux.Handle("/ui/", http.StripPrefix("/ui/", uiHandler))

			// 根路径与非 API 路径全量映射 WebUI 静态文件，确保虚拟域名模式下所有 API 与静态文件无缝响应
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/metrics") {
					uiHandler.ServeHTTP(w, r)
				} else {
					http.NotFound(w, r)
				}
			})
		}

		mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
		if mode == "" {
			mode = "both"
		}

		enablePort := (mode == "port" || mode == "both")
		enableDomain := (mode == "domain" || mode == "both")

		listenAddr := cfg.Addr
		if listenAddr == "" {
			listenAddr = defaultPortAddr
		}

		if enableDomain && cfg.Domain != "" {
			cleanDomain := strings.TrimSuffix(cfg.Domain, ".")
			zap.S().Infof("🎨 WebUI Mode [%s]: Virtual Domain Active -> http://%s/ & https://%s/ (Zero exposed ports)", strings.ToUpper(mode), cleanDomain, cleanDomain)
		}

		if enablePort && listenAddr != "" {
			zap.S().Infof("🎨 WebUI Mode [%s]: HTTP Port Listening -> http://%s/ui/", strings.ToUpper(mode), listenAddr)
		}
	}

	registerStateRoutes(mux)
	registerMetricsRoutes(mux)
	registerAnalyticsRoutes(mux)
	registerConfigRoutes(mux)
	registerControlRoutes(mux)
	registerAuthRoutes(mux)
	registerLogRoutes(mux)
	registerConnectionRoutes(mux)

	globalWebUIMux = mux
	return mux
}

func handleVirtualDomainConn(conn net.Conn) {
	if globalWebUIMux == nil {
		conn.Close()
		return
	}

	bufConn := newBufferedConn(conn)
	firstByte, err := bufConn.PeekByte()
	if err != nil {
		conn.Close()
		return
	}

	// 0x16 即 TLS Handshake Client Hello 头部，自动兼容现代浏览器默认发起的 HTTPS 请求
	if firstByte == 0x16 {
		tlsCfg := getVirtualDomainTLSConfig(cfg.UI.Domain)
		if tlsCfg == nil {
			conn.Close()
			return
		}
		tlsConn := tls.Server(bufConn, tlsCfg)
		if err := tlsConn.Handshake(); err != nil {
			zap.S().Debugf("[WebUI] Virtual domain TLS handshake warning: %v", err)
			conn.Close()
			return
		}
		serveHTTPConn(tlsConn)
	} else {
		serveHTTPConn(bufConn)
	}
}

func serveHTTPConn(conn net.Conn) {
	listener := newSingleConnListener(conn)
	defer listener.Close()
	_ = http.Serve(listener, globalWebUIMux)
}
