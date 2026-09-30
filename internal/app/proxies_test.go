package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"nodebridge/internal/store"
	"nodebridge/internal/transport"
)

type proxyFixture struct {
	*fixture
	node   *Node
	local  *httptest.Server
	config Config
	target *httptest.Server
	port   int
}

func newProxyFixture(t *testing.T, count int) *proxyFixture {
	t.Helper()
	base := portPool(t, count)
	f := newFixture(t, base, base+count-1)
	ssh, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ssh.Close() })
	go func() {
		for {
			c, err := ssh.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow()
			for {
				kind, data, err := c.Read(r.Context())
				if err != nil {
					return
				}
				if c.Write(r.Context(), kind, data) != nil {
					return
				}
			}
		}
		w.Header().Set("X-Original-Path", r.URL.RequestURI())
		w.Header().Set("X-Original-Host", r.Host)
		w.Header().Set("X-Original-Cookie", r.Header.Get("Cookie"))
		http.SetCookie(w, &http.Cookie{Name: "overleaf", Value: "example", Path: "/"})
		io.Copy(w, r.Body)
	}))
	t.Cleanup(target.Close)
	dir := t.TempDir()
	c, err := InitNode(dir, "127.0.0.1:9899", ssh.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	n := NewNode(ctx, dir, c)
	if err := n.Pair(f.invite(t, "Overleaf 节点")); err != nil {
		t.Fatal(err)
	}
	c, err = LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { n.Run(); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	local := httptest.NewServer(n.Handler())
	t.Cleanup(local.Close)
	p := &proxyFixture{fixture: f, node: n, local: local, config: c, target: target, port: target.Listener.Addr().(*net.TCPAddr).Port}
	waitFor(t, func() bool {
		f.hub.mu.Lock()
		defer f.hub.mu.Unlock()
		l := f.hub.live[c.NodeID]
		return f.hub.nodeOnline(l) && l.status.TCPEnabled && l.status.SSHReady
	})
	return p
}

func (f *proxyFixture) localRequest(t *testing.T, method, path string, data any, want int) {
	t.Helper()
	b, _ := json.Marshal(data)
	r, _ := http.NewRequest(method, f.local.URL+path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-NodeBridge-Request", "1")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(resp)
	if resp.StatusCode != want {
		t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
	}
}

func (f *proxyFixture) authorize(t *testing.T) {
	t.Helper()
	f.localRequest(t, "POST", "/api/tcp-ports", map[string]int{"port": f.port}, 200)
	waitFor(t, func() bool {
		f.hub.mu.Lock()
		defer f.hub.mu.Unlock()
		a, r := tcpPortStatus(f.hub.live[f.config.NodeID], f.port)
		return a && r
	})
}

func (f *proxyFixture) create(t *testing.T, publicPort int, want int) store.Proxy {
	t.Helper()
	r := f.request(t, "POST", "/api/proxies", map[string]any{"name": "Overleaf", "node_id": f.config.NodeID, "target_port": f.port, "port": publicPort}, f.cookie)
	defer r.Body.Close()
	if r.StatusCode != want {
		t.Fatalf("create: %d %s", r.StatusCode, readBody(r))
	}
	var p store.Proxy
	if want == 201 {
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func mustStatus(t *testing.T, r *http.Response, want int) {
	t.Helper()
	body := readBody(r)
	if r.StatusCode != want {
		t.Fatalf("status: %d %s", r.StatusCode, body)
	}
}

func openEcho(t *testing.T, port int) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(port)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(3 * time.Second))
	echoBytes(t, c)
	return c
}

func echoBytes(t *testing.T, c net.Conn) {
	t.Helper()
	if _, err := c.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	var b [5]byte
	if _, err := io.ReadFull(c, b[:]); err != nil || string(b[:]) != "hello" {
		t.Fatalf("echo: %q %v", b, err)
	}
}

func (f *proxyFixture) forbiddenStream(t *testing.T, port int) {
	t.Helper()
	f.hub.mu.Lock()
	s := f.hub.live[f.config.NodeID].session
	f.hub.mu.Unlock()
	c, err := s.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(time.Second))
	b := [3]byte{transport.TCP}
	binary.BigEndian.PutUint16(b[1:], uint16(port))
	if _, err := c.Write(b[:]); err != nil {
		t.Fatal(err)
	}
	var ack [1]byte
	if _, err := io.ReadFull(c, ack[:]); err != nil || ack[0] != 1 {
		t.Fatalf("unauthorized port %d: ack %d, %v", port, ack[0], err)
	}
}

func TestTCPProxyHTTPWebSocketAndIndependentPause(t *testing.T) {
	f := newProxyFixture(t, 4)
	f.create(t, 0, 400) // Hub cannot publish a port that the node did not authorize.
	f.forbiddenStream(t, f.port)
	f.forbiddenStream(t, f.config.SSHPort)
	f.forbiddenStream(t, 9899)
	for _, port := range []int{0, 65536, f.config.SSHPort, 9899} {
		f.localRequest(t, "POST", "/api/tcp-ports", map[string]int{"port": port}, 400)
	}
	f.authorize(t)
	p := f.create(t, 0, 201)
	f.create(t, 0, 400) // Duplicate backend rejected without allocating another port.
	address := "127.0.0.1:" + itoa(p.Port)
	payload := strings.Repeat("Overleaf upload \x00", 60000)
	req, _ := http.NewRequest("POST", "http://"+address+"/project/upload?name=test.tex", strings.NewReader(payload))
	req.Header.Set("Cookie", "overleaf-login=kept")
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	r, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != 200 || r.Header.Get("X-Original-Path") != "/project/upload?name=test.tex" || r.Header.Get("X-Original-Host") != address || r.Header.Get("X-Original-Cookie") != "overleaf-login=kept" || len(r.Cookies()) != 1 {
		t.Fatalf("HTTP headers changed: %+v", r.Header)
	}
	if data := readBody(r); data != payload {
		t.Fatalf("large upload/response changed: %d", len(data))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	openWS := func(t *testing.T) *websocket.Conn {
		t.Helper()
		c, _, err := websocket.Dial(ctx, "ws://"+address+"/ws", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.CloseNow() })
		if err := c.Write(ctx, websocket.MessageText, []byte("editor sync")); err != nil {
			t.Fatal(err)
		}
		_, b, err := c.Read(ctx)
		if err != nil || string(b) != "editor sync" {
			t.Fatalf("WebSocket: %q %v", b, err)
		}
		return c
	}
	for _, scope := range []string{"proxy", "node"} {
		t.Run(scope, func(t *testing.T) {
			ssh := openEcho(t, f.config.PublicPort)
			ws := openWS(t)
			if scope == "proxy" {
				mustStatus(t, f.request(t, "PUT", "/api/proxies/"+p.ID+"/forwarding", map[string]bool{"paused": true}, f.cookie), 200)
			} else {
				f.localRequest(t, "PUT", "/api/tcp-forwarding", map[string]bool{"paused": true}, 200)
				f.forbiddenStream(t, f.port)
			}
			readCtx, stop := context.WithTimeout(ctx, time.Second)
			_, _, err := ws.Read(readCtx)
			timedOut := readCtx.Err() != nil
			stop()
			if err == nil || timedOut {
				t.Fatalf("TCP pause did not promptly disconnect WebSocket: %v", err)
			}
			echoBytes(t, ssh) // Existing SSH survives both kinds of TCP pause.
			if _, resp, err := websocket.Dial(ctx, "ws://"+address+"/ws", nil); err == nil {
				if resp != nil {
					resp.Body.Close()
				}
				t.Fatal("new TCP connection bypassed pause")
			}
			if scope == "proxy" {
				mustStatus(t, f.request(t, "PUT", "/api/proxies/"+p.ID+"/forwarding", map[string]bool{"paused": false}, f.cookie), 200)
			} else {
				f.localRequest(t, "PUT", "/api/tcp-forwarding", map[string]bool{"paused": false}, 200)
				waitFor(t, func() bool {
					f.hub.mu.Lock()
					defer f.hub.mu.Unlock()
					return !f.hub.live[f.config.NodeID].status.TCPPaused
				})
			}
			openWS(t).CloseNow()
		})
	}
	ws := openWS(t)
	for _, path := range []string{"/api/forwarding", "/api/nodes/" + f.config.NodeID + "/forwarding"} {
		mustStatus(t, f.request(t, "PUT", path, map[string]bool{"paused": true}, f.cookie), 200)
		if err := ws.Write(ctx, websocket.MessageText, []byte("unaffected")); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ws.Read(ctx); err != nil {
			t.Fatalf("SSH pause affected TCP: %v", err)
		}
		mustStatus(t, f.request(t, "PUT", path, map[string]bool{"paused": false}, f.cookie), 200)
	}
	f.localRequest(t, "PUT", "/api/forwarding", map[string]bool{"paused": true}, 200)
	if err := ws.Write(ctx, websocket.MessageText, []byte("local SSH pause")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.Read(ctx); err != nil {
		t.Fatalf("local SSH pause affected TCP: %v", err)
	}
	f.localRequest(t, "PUT", "/api/forwarding", map[string]bool{"paused": false}, 200)
	f.localRequest(t, "DELETE", "/api/tcp-ports/"+itoa(f.port), nil, 200)
	readCtx, stop := context.WithTimeout(ctx, time.Second)
	_, _, err = ws.Read(readCtx)
	timedOut := readCtx.Err() != nil
	stop()
	if err == nil || timedOut {
		t.Fatalf("authorization revoke did not close stream: %v", err)
	}
	f.forbiddenStream(t, f.port)
	c, err := LoadConfig(f.node.dir)
	if err != nil || len(c.AllowedTCPPorts) != 0 {
		t.Fatalf("revocation not persisted: %+v %v", c, err)
	}
	f.authorize(t)
	openWS(t).CloseNow()
	mustStatus(t, f.request(t, "DELETE", "/api/proxies/"+p.ID, nil, f.cookie), 200)
	f.create(t, p.Port, 201) // Deleting the proxy frees its exact port.
	mustStatus(t, f.request(t, "DELETE", "/api/nodes/"+f.config.NodeID, nil, f.cookie), 200)
	if err := f.hub.Store.View(func(s store.State) error {
		if len(s.Proxies) != 0 {
			t.Error("node removal left orphan proxies")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTCPProxyAllocationPersistenceAndAuthorization(t *testing.T) {
	f := newProxyFixture(t, 2)
	f.authorize(t)
	f.create(t, f.config.PublicPort, 400) // SSH and TCP share the port pool.
	occupied, err := net.Listen("tcp", ":"+itoa(f.config.PublicPort+1))
	if err != nil {
		t.Fatal(err)
	}
	f.create(t, 0, 400)
	occupied.Close()
	p := f.create(t, 0, 201)
	mustStatus(t, f.request(t, "PUT", "/api/proxies/"+p.ID+"/forwarding", map[string]bool{"paused": true}, f.cookie), 200)
	f.localRequest(t, "PUT", "/api/tcp-forwarding", map[string]bool{"paused": true}, 200)
	loaded, err := LoadConfig(f.node.dir)
	if err != nil || !loaded.TCPForwardingPaused || !slices.Contains(loaded.AllowedTCPPorts, f.port) {
		t.Fatalf("node settings not saved: %+v %v", loaded, err)
	}
	if err := f.hub.Store.Update(func(s *store.State) error {
		s.Users["member"] = store.User{Username: "member"}
		s.Sessions[hashToken("member-proxy")] = store.Session{Username: "member", Expires: time.Now().Add(time.Hour)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []struct{ method, path string }{{"GET", "/api/proxies"}, {"POST", "/api/proxies"}, {"PUT", "/api/proxies/" + p.ID + "/forwarding"}, {"DELETE", "/api/proxies/" + p.ID}} {
		mustStatus(t, f.request(t, endpoint.method, endpoint.path, map[string]bool{"paused": false}, nil), 401)
		mustStatus(t, f.request(t, endpoint.method, endpoint.path, map[string]bool{"paused": false}, &http.Cookie{Name: "nodebridge_session", Value: "member-proxy"}), 403)
	}
	if err := f.hub.Close(); err != nil {
		t.Fatal(err)
	}
	occupied, err = net.Listen("tcp", ":"+itoa(p.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	h, err := NewHub(f.ctx, f.dir, f.hub.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.mu.Lock()
	restored := h.proxies[p.ID]
	if restored == nil || !restored.saved.Paused || restored.saved.Port != p.Port || restored.listener != nil || restored.error == "" {
		t.Fatalf("failed restoration not visible: %+v", restored)
	}
	h.mu.Unlock()
	// Releasing the conflict then resuming retries the same public port.
	occupied.Close()
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	body := bytes.NewBufferString(`{"paused":false}`)
	req, _ := http.NewRequest("PUT", srv.URL+"/api/proxies/"+p.ID+"/forwarding", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-NodeBridge-Request", "1")
	req.AddCookie(f.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, resp, 200)
	h.mu.Lock()
	if restored.listener == nil || restored.error != "" || restored.saved.Port != p.Port || restored.saved.Paused {
		t.Error("resuming did not restore the original port")
	}
	h.mu.Unlock()
}
