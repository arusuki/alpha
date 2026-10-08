package bastion

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"time"

	"project-alpha/internal/httpapi"
)

// proxyTarget is a fixed upstream, never a destination supplied by a client.
func proxyTarget(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Scheme != "http" && u.Scheme != "https" || u.Path != "" && u.Path != "/" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("总控地址需为不含账号、路径或查询参数的 HTTP/HTTPS 地址")
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("总控端口无效")
		}
	}
	return u, nil
}
func newShareProxy(c installation) (http.Handler, func(), error) {
	target, err := proxyTarget(c.ControlURL)
	if err != nil {
		return nil, nil, err
	}
	if target.Host == netAddress(c.ListenHost, c.StatusPort) {
		return nil, nil, fmt.Errorf("总控代理目标不能指向分享节点自身的入口")
	}
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 60 * time.Second, IdleConnTimeout: 60 * time.Second, MaxIdleConns: 128, MaxIdleConnsPerHost: 32}
	proxy := &httputil.ReverseProxy{
		Rewrite:       func(r *httputil.ProxyRequest) { r.SetURL(target); r.Out.Host = r.In.Host },
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
			log.Printf("share node HTTP proxy: %v", e)
			http.Error(w, "无法连接总控，请检查总控内网 Web 地址", http.StatusBadGateway)
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Preserve the public Host and Origin together so control's existing Host,
		// Origin, CSRF and bearer-token checks also protect the proxy entrance.
		if r.Host != netAddress(c.ListenHost, c.StatusPort) || r.URL.IsAbs() {
			http.Error(w, "分享节点入口地址无效", http.StatusBadRequest)
			return
		}
		if !httpapi.MemberEntranceAllowed(r) {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "分享入口仅开放使用者 status 面板，请打开 /status/你的使用者标识", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, r)
	})
	return handler, transport.CloseIdleConnections, nil
}
func serveShareProxy(ctx context.Context, s keyStore) error {
	r, c, err := s.open()
	if err != nil {
		return err
	}
	r.Close()
	if os.Geteuid() != c.WorkerUID {
		return fmt.Errorf("share node HTTP 代理必须以 alpha-worker 运行")
	}
	handler, closeTransport, err := newShareProxy(c)
	if err != nil {
		return err
	}
	defer closeTransport()
	listener, err := net.Listen("tcp", netAddress(c.ListenHost, c.StatusPort))
	if err != nil {
		return err
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 65536}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if server.Shutdown(shutdown) != nil {
				server.Close()
			}
		case <-done:
		}
	}()
	log.Printf("share node HTTP proxy: %s -> %s", listener.Addr(), c.ControlURL)
	if err = server.Serve(listener); err == http.ErrServerClosed {
		return nil
	}
	return err
}

const proxyUnitPath = "/etc/systemd/system/project-alpha-share-node.service"
const proxyUnitName = "project-alpha-share-node.service"

func proxyUnit() []byte {
	return []byte(`[Unit]
Description=project alpha share node HTTP proxy
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=alpha-worker
Group=alpha-worker
ExecStart=/usr/local/libexec/project-alpha-jump share-node --serve
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
UMask=0077

[Install]
WantedBy=multi-user.target
`)
}
