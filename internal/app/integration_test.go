package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"nodebridge/internal/store"
)

type fixture struct {
	hub    *Hub
	server *httptest.Server
	client *http.Client
	cookie *http.Cookie
	dir    string
	ctx    context.Context
	cancel context.CancelFunc
}

func newFixture(t *testing.T, start, end int) *fixture {
	t.Helper()
	dir := t.TempDir()
	c, err := InitHub(dir, ":0", "https://127.0.0.1:9443", start, end)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h, err := NewHub(ctx, dir, c)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h.Handler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	h.Config.PublicURL = srv.URL
	f := &fixture{hub: h, server: srv, client: pinnedClient(h.Fingerprint), dir: dir, ctx: ctx, cancel: cancel}
	t.Cleanup(func() { cancel(); h.Close(); srv.Close() })
	b, err := os.ReadFile(filepath.Join(dir, "initial-admin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	password := strings.TrimSpace(strings.Split(string(b), "password: ")[1])
	resp := f.request(t, "POST", "/api/login", map[string]any{"username": "admin", "password": password}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %s", readBody(resp))
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "nodebridge_session" {
			f.cookie = cookie
		}
	}
	resp.Body.Close()
	if f.cookie == nil {
		t.Fatal("missing session cookie")
	}
	return f
}

func (f *fixture) request(t *testing.T, method, path string, body any, cookie *http.Cookie) *http.Response {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest(method, f.server.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-NodeBridge-Request", "1")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readBody(r *http.Response) string {
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func (f *fixture) invite(t *testing.T, name string) string {
	t.Helper()
	r := f.request(t, "POST", "/api/invites", map[string]string{"name": name}, f.cookie)
	defer r.Body.Close()
	if r.StatusCode != 201 {
		t.Fatalf("invite: %s", readBody(r))
	}
	var v struct {
		Link string `json:"link"`
	}
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v.Link
}

func portPool(t *testing.T, count int) int {
	t.Helper()
	for tries := 0; tries < 100; tries++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		base := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		if base+count > 65535 {
			continue
		}
		listeners := []net.Listener{}
		ok := true
		for p := base; p < base+count; p++ {
			l, err := net.Listen("tcp", net.JoinHostPort("", itoa(p)))
			if err != nil {
				ok = false
				break
			}
			listeners = append(listeners, l)
		}
		for _, l := range listeners {
			l.Close()
		}
		if ok {
			return base
		}
	}
	t.Fatal("no contiguous test ports")
	return 0
}

func itoa(n int) string { return strconv.Itoa(n) }

func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestPairForwardReconnectAndRevoke(t *testing.T) {
	base := portPool(t, 4)
	f := newFixture(t, base, base+3)
	ssh, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ssh.Close()
	go func() {
		for {
			c, err := ssh.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	dir := t.TempDir()
	c, err := InitNode(dir, "127.0.0.1:9899", ssh.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	nctx, ncancel := context.WithCancel(f.ctx)
	defer ncancel()
	n := NewNode(nctx, dir, c)
	if err = n.Pair(f.invite(t, "实验室")); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	paired := n.config
	n.mu.Unlock()
	if paired.PublicPort != base {
		t.Fatalf("unexpected assigned port %d", paired.PublicPort)
	}
	if err = n.Pair(f.invite(t, "重复")); err == nil {
		t.Fatal("configured node allowed repair")
	}
	done := make(chan struct{})
	go func() { n.Run(); close(done) }()
	defer func() { ncancel(); <-done }()
	ready := func() bool {
		f.hub.mu.Lock()
		defer f.hub.mu.Unlock()
		l := f.hub.live[paired.NodeID]
		return l != nil && l.session != nil && !l.session.IsClosed() && !l.seen.IsZero() && l.status.SSHReady
	}
	waitFor(t, ready)
	if err = f.hub.sampleHealth(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err = f.hub.Store.View(func(s store.State) error {
		n := s.Nodes[paired.NodeID]
		d := s.Daily[paired.NodeID+"/"+time.Now().UTC().Format("2006-01-02")]
		if n.LastSeen.IsZero() || d.Checks != 1 || d.Online != 1 || d.SSHReady != 1 {
			t.Errorf("health not persisted: %+v, %+v", n, d)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A payload larger than the yamux stream window exercises flow control.
	payload := bytes.Repeat([]byte("nodebridge-ssh-test\n"), 40000)
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(paired.PublicPort)))
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(8 * time.Second))
	written := make(chan error, 1)
	go func() {
		_, err := conn.Write(payload)
		if err == nil {
			err = conn.(*net.TCPConn).CloseWrite()
		}
		written <- err
	}()
	got, err := io.ReadAll(conn)
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = <-written; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("forward payload mismatch: got %d want %d", len(got), len(payload))
	}
	f.hub.mu.Lock()
	old := f.hub.live[paired.NodeID].session
	old.Close()
	f.hub.mu.Unlock()
	waitFor(t, func() bool {
		f.hub.mu.Lock()
		defer f.hub.mu.Unlock()
		l := f.hub.live[paired.NodeID]
		return l.session != nil && l.session != old && !l.seen.IsZero()
	})
	r := f.request(t, "GET", "/api/nodes", nil, f.cookie)
	body := readBody(r)
	if r.StatusCode != 200 || !strings.Contains(body, "实验室") || strings.Contains(body, "credential") || !strings.Contains(body, `"checks":1`) {
		t.Fatalf("nodes: %s", body)
	}
	loaded, err := LoadConfig(dir)
	if err != nil || loaded.NodeID != paired.NodeID || loaded.PublicPort != paired.PublicPort {
		t.Fatalf("configuration not persisted: %v", err)
	}
	r = f.request(t, "DELETE", "/api/nodes/"+paired.NodeID, map[string]any{}, f.cookie)
	if r.StatusCode != 200 {
		t.Fatal(readBody(r))
	}
	r.Body.Close()
	_, err = net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(paired.PublicPort)), time.Second)
	if err == nil {
		t.Fatal("revoked listener still accepting connections")
	}
	headers := http.Header{}
	headers.Set("X-Node-ID", paired.NodeID)
	headers.Set("Authorization", "Bearer "+paired.Credential)
	req, _ := http.NewRequest("GET", f.server.URL+"/api/connect", nil)
	req.Header = headers
	r, err = f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != 401 {
		t.Fatal(readBody(r))
	}
	r.Body.Close()
}

func TestInviteAtomicityPortCollisionAndExpiration(t *testing.T) {
	base := portPool(t, 3)
	f := newFixture(t, base, base+2)
	blocked, err := net.Listen("tcp", net.JoinHostPort("", itoa(base)))
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	link := f.invite(t, "node-one")
	pair, err := DecodePair(link)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := f.request(t, "POST", "/api/enroll", map[string]string{"token": pair.Token}, nil)
			codes <- r.StatusCode
			r.Body.Close()
		}()
	}
	wg.Wait()
	close(codes)
	success, denied := 0, 0
	for code := range codes {
		if code == 201 {
			success++
		}
		if code == 400 {
			denied++
		}
	}
	if success != 1 || denied != 1 {
		t.Fatalf("single use was not atomic: %d success, %d denied", success, denied)
	}
	var id string
	f.hub.Store.View(func(s store.State) error {
		for _, n := range s.Nodes {
			id = n.ID
			if n.Port != base+1 {
				t.Errorf("occupied port was assigned: %d", n.Port)
			}
		}
		return nil
	})
	p2, _ := DecodePair(f.invite(t, "node-two"))
	r := f.request(t, "POST", "/api/enroll", map[string]string{"token": p2.Token}, nil)
	if r.StatusCode != 201 {
		t.Fatal(readBody(r))
	}
	r.Body.Close()
	p3, _ := DecodePair(f.invite(t, "node-three"))
	r = f.request(t, "POST", "/api/enroll", map[string]string{"token": p3.Token}, nil)
	if r.StatusCode != 400 {
		t.Fatal(readBody(r))
	}
	r.Body.Close()
	r = f.request(t, "DELETE", "/api/nodes/"+id, map[string]any{}, f.cookie)
	r.Body.Close()
	r = f.request(t, "POST", "/api/enroll", map[string]string{"token": p3.Token}, nil)
	if r.StatusCode != 201 {
		t.Fatalf("failed allocation consumed invite: %s", readBody(r))
	}
	r.Body.Close()
	p4, _ := DecodePair(f.invite(t, "expired"))
	f.hub.Store.Update(func(s *store.State) error {
		v := s.Invites[hashToken(p4.Token)]
		v.Expires = time.Now().Add(-time.Second)
		s.Invites[hashToken(p4.Token)] = v
		return nil
	})
	r = f.request(t, "POST", "/api/enroll", map[string]string{"token": p4.Token}, nil)
	if r.StatusCode != 400 {
		t.Fatal(readBody(r))
	}
	r.Body.Close()
}

func TestAuthenticationAndLocalBoundaries(t *testing.T) {
	base := portPool(t, 2)
	f := newFixture(t, base, base+1)
	for _, tc := range []struct {
		method, path string
		body         any
		code         int
	}{
		{"GET", "/api/nodes", nil, 401}, {"POST", "/api/register", map[string]any{"username": "member", "password": "a-long-password", "admin": true}, 400},
	} {
		r := f.request(t, tc.method, tc.path, tc.body, nil)
		if r.StatusCode != tc.code {
			t.Fatalf("%s: %s", tc.path, readBody(r))
		}
		r.Body.Close()
	}
	r := f.request(t, "PUT", "/api/settings", map[string]bool{"allow_register": true}, f.cookie)
	r.Body.Close()
	r = f.request(t, "POST", "/api/register", map[string]any{"username": "member", "password": "a-long-password", "admin": true}, nil)
	if r.StatusCode != 201 {
		t.Fatal(readBody(r))
	}
	r.Body.Close()
	r = f.request(t, "POST", "/api/login", map[string]string{"username": "member", "password": "a-long-password"}, nil)
	var cookie *http.Cookie
	for _, c := range r.Cookies() {
		if c.Name == "nodebridge_session" {
			cookie = c
		}
	}
	r.Body.Close()
	if cookie == nil {
		t.Fatal("member login failed")
	}
	r = f.request(t, "POST", "/api/invites", map[string]string{"name": "forbidden"}, cookie)
	if r.StatusCode != 403 {
		t.Fatal(readBody(r))
	}
	r.Body.Close()
	r = f.request(t, "GET", "/api/users", nil, cookie)
	if r.StatusCode != 403 {
		t.Fatal(readBody(r))
	}
	r.Body.Close()
	r = f.request(t, "DELETE", "/api/users/admin", map[string]any{}, f.cookie)
	if r.StatusCode != 400 {
		t.Fatal(readBody(r))
	}
	r.Body.Close()
	req, _ := http.NewRequest("POST", f.server.URL+"/api/invites", strings.NewReader(`{"name":"csrf"}`))
	req.AddCookie(f.cookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-NodeBridge-Request", "1")
	req.Header.Set("Origin", "https://evil.example")
	r, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != 403 {
		t.Fatal(readBody(r))
	}
	r.Body.Close()
	req, _ = http.NewRequest("POST", f.server.URL+"/api/logout", strings.NewReader(`{}`))
	req.AddCookie(f.cookie)
	req.Header.Set("Content-Type", "application/json")
	r, err = f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != 403 {
		t.Fatal(readBody(r))
	}
	r.Body.Close()
	r = f.request(t, "PATCH", "/api/users/member", map[string]string{"password": "another-long-password"}, f.cookie)
	r.Body.Close()
	r = f.request(t, "GET", "/api/nodes", nil, cookie)
	if r.StatusCode != 401 {
		t.Fatal(readBody(r))
	}
	r.Body.Close()
	bad := pinnedClient(strings.Repeat("0", 64))
	if resp, err := bad.Get(f.server.URL + "/api/health"); err == nil {
		resp.Body.Close()
		t.Fatal("wrong certificate fingerprint was accepted")
	}
	if _, err = InitNode(t.TempDir(), "0.0.0.0:9899", 22); err == nil {
		t.Fatal("public maintenance listener accepted")
	}
	unsafeDir := t.TempDir()
	unsafeConfig := Config{Mode: "node", Listen: "0.0.0.0:9899", SSHPort: 22}
	if err = WriteJSON(filepath.Join(unsafeDir, "config.json"), unsafeConfig); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadConfig(unsafeDir); err == nil {
		t.Fatal("public maintenance listener accepted from persisted configuration")
	}
	c, err := InitNode(t.TempDir(), "127.0.0.1:9899", 22)
	if err != nil {
		t.Fatal(err)
	}
	n := NewNode(f.ctx, t.TempDir(), c)
	recorder := httptest.NewRecorder()
	localReq := httptest.NewRequest("GET", "http://evil.example/api/node", nil)
	n.Handler().ServeHTTP(recorder, localReq)
	if recorder.Code != 403 {
		t.Fatal("DNS rebinding host accepted")
	}
	recorder = httptest.NewRecorder()
	localReq = httptest.NewRequest("POST", "http://127.0.0.1:9899/api/pair", strings.NewReader(`{"link":"invalid"}`))
	localReq.Header.Set("Content-Type", "application/json")
	n.Handler().ServeHTTP(recorder, localReq)
	if recorder.Code != 403 {
		t.Fatal("local CSRF request accepted")
	}
}

func TestHubRestartKeepsAssignedPort(t *testing.T) {
	base := portPool(t, 2)
	f := newFixture(t, base, base+1)
	pair, _ := DecodePair(f.invite(t, "persistent"))
	r := f.request(t, "POST", "/api/enroll", map[string]string{"token": pair.Token}, nil)
	var result struct {
		NodeID string `json:"node_id"`
		Port   int    `json:"port"`
	}
	json.NewDecoder(r.Body).Decode(&result)
	r.Body.Close()
	c := f.hub.Config
	f.cancel()
	f.hub.Close()
	f.server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := NewHub(ctx, f.dir, c)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.mu.Lock()
	l := h.live[result.NodeID]
	h.mu.Unlock()
	if l == nil || l.listener == nil || l.listener.Addr().(*net.TCPAddr).Port != result.Port {
		t.Fatal("restart did not restore assigned port")
	}
}

func TestConfiguredCertificateFingerprint(t *testing.T) {
	dir := t.TempDir()
	c, err := InitHub(dir, ":0", "https://127.0.0.1:9443", 30000, 30009)
	if err != nil {
		t.Fatal(err)
	}
	cert, key, expected, err := certificate(t.TempDir(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	c.CertFile = cert
	c.KeyFile = key
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := NewHub(ctx, dir, c)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.Fingerprint != expected {
		t.Fatal("pairing fingerprint did not match the configured TLS certificate")
	}
}
