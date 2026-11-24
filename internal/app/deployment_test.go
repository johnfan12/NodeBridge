package app

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

func TestNginxLocationUsesHTTPSAndWebSockets(t *testing.T) {
	c := Config{Mode: "hub", Listen: ":9443", PublicURL: "https://ssh.example.com"}
	text, err := NginxLocation(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"proxy_pass https://127.0.0.1:9443;", "Host $http_host;", "Upgrade $http_upgrade;", "Connection \"upgrade\";", "proxy_ssl_server_name on;", "proxy_ssl_name ssh.example.com;", "proxy_read_timeout 3600s;"} {
		if !strings.Contains(text, required) {
			t.Errorf("Nginx missing %q", required)
		}
	}
	for _, invalid := range []Config{
		{Mode: "node", Listen: "127.0.0.1:9899", PublicURL: c.PublicURL},
		{Mode: "hub", Listen: "203.0.113.10:9443", PublicURL: c.PublicURL},
		{Mode: "hub", Listen: ":9443", PublicURL: "https://ssh.example.com;break"},
	} {
		if _, err := NginxLocation(invalid); err == nil {
			t.Fatal("unsafe Nginx backend/hostname accepted")
		}
	}
}

func TestDeploymentCheckDetectsHTTPToTLSProxy(t *testing.T) {
	base := portPool(t, 1)
	f := newFixture(t, base, base)
	if err := CheckPublicEndpoint(context.Background(), f.hub.Config); err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(strings.Replace(f.server.URL, "https://", "http://", 1))
	if err != nil {
		t.Fatal(err)
	}
	badProxy := httputil.NewSingleHostReverseProxy(target)
	cert, err := tls.LoadX509KeyPair(f.hub.Config.CertFile, f.hub.Config.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(badProxy)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()
	c := f.hub.Config
	c.PublicURL = srv.URL
	if err := CheckPublicEndpoint(context.Background(), c); err == nil || !strings.Contains(err.Error(), "proxy_pass https://") {
		t.Fatalf("HTTP upstream mismatch was not diagnosed: %v", err)
	}
	static := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("not NodeBridge")) }))
	static.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	static.StartTLS()
	defer static.Close()
	c.PublicURL = static.URL
	if err := CheckPublicEndpoint(context.Background(), c); err == nil {
		t.Fatal("non-NodeBridge HTTP 200 mistaken for a working deployment")
	}
}
