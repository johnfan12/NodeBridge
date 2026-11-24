package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
)

// The generated fragment belongs inside the existing HTTPS server block, so it
// never replaces the operator's domain certificates or unrelated virtual hosts.
func NginxLocation(c Config) (string, error) {
	if c.Mode != "hub" {
		return "", errors.New("Nginx 配置只适用于 hub")
	}
	u, err := validateURL(c.PublicURL)
	if err != nil {
		return "", err
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9.:-]+$`).MatchString(u.Hostname()) {
		return "", errors.New("请使用有效的 IP 或 ASCII 域名生成 Nginx 配置")
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("Hub 监听端口无效")
	}
	switch host {
	case "", "0.0.0.0", "localhost":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return "", errors.New("生成的 Nginx 配置仅支持本机回环后端，请让 Hub 监听 :端口 或 127.0.0.1:端口")
	}
	return fmt.Sprintf(`# Include inside your existing HTTPS server block; replace its old location /.
# Hub speaks HTTPS, including on loopback. Keep the public certificate in Nginx.
location / {
    proxy_pass https://%s;
    proxy_http_version 1.1;
    proxy_set_header Host $http_host;
    proxy_set_header X-Forwarded-Proto https;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_ssl_server_name on;
    proxy_ssl_name %s;
    # Only the local loopback hop uses the Hub's self-signed certificate.
    proxy_ssl_verify off;
    proxy_buffering off;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
    proxy_redirect off;
}
`, net.JoinHostPort(host, port), u.Hostname()), nil
}

// Verify the externally configured route, not just a successful localhost bind.
func CheckPublicEndpoint(ctx context.Context, c Config) error {
	if c.Mode != "hub" {
		return errors.New("公网部署检查只适用于 hub")
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return err
	}
	client, err := clientForHub(c.PublicURL, hashBytes(cert.Certificate[0]), TLSAuto)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	for _, path := range []string{"/api/health", "/api/connect"} {
		req, err := http.NewRequestWithContext(ctx, "GET", c.PublicURL+path, nil)
		if err != nil {
			return err
		}
		r, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("公网地址 %s%s 不可用，请检查 DNS、域名证书和 Nginx HTTPS 代理: %w", c.PublicURL, path, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(r.Body, 16384))
		r.Body.Close()
		if readErr != nil {
			return readErr
		}
		if path == "/api/connect" && r.StatusCode == http.StatusUnauthorized {
			continue // Expected: this probe intentionally carries no node credential.
		}
		var health struct {
			OK   bool   `json:"ok"`
			Mode string `json:"mode"`
		}
		if path == "/api/health" && r.StatusCode == 200 && json.Unmarshal(data, &health) == nil && health.OK && health.Mode == "hub" {
			continue
		}
		return fmt.Errorf("公网 %s 返回非预期响应 HTTP %d；Hub 的 9443 是 HTTPS，Nginx 必须使用 proxy_pass https://127.0.0.1:9443（自定义端口按实际修改），并代理全部 /api/ 路径", path, r.StatusCode)
	}
	return nil
}
