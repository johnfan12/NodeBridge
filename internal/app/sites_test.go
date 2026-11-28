package app

import (
	"context"
	"encoding/json"
	"github.com/coder/websocket"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"nodebridge/internal/store"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagedOverleafPairAndForward(t *testing.T) {
	start := portPool(t, 2)
	f := newFixture(t, start, start+1)
	f.hub.Store.Update(func(st *store.State) error {
		st.Users["reader"] = store.User{Username: "reader"}
		st.Sessions[hashToken("reader")] = store.Session{Username: "reader", Expires: time.Now().Add(time.Hour)}
		return nil
	})
	reader := &http.Cookie{Name: "nodebridge_session", Value: "reader"}
	for _, op := range [][2]string{{"GET", "/api/sites"}, {"POST", "/api/sites"}, {"POST", "/api/sites/lab.example.org/invite"}, {"DELETE", "/api/sites/lab.example.org/pair"}} {
		r := f.request(t, op[0], op[1], nil, reader)
		r.Body.Close()
		if r.StatusCode != 403 {
			t.Fatalf("%s: %d", op, r.StatusCode)
		}
	}
	f.hub.siteSocket = filepath.Join(t.TempDir(), "site.sock")
	ln, e := net.Listen("unix", f.hub.siteSocket)
	if e != nil {
		t.Fatal(e)
	}
	defer ln.Close()
	provisioned := make(chan siteRequest, 1)
	go func() {
		c, e := ln.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		var p siteRequest
		json.NewDecoder(c).Decode(&p)
		provisioned <- p
		io.WriteString(c, "{}\n")
	}()
	r := f.request(t, "POST", "/api/sites", map[string]string{"domain": "lab.example.org"}, f.cookie)
	if r.StatusCode != 201 {
		t.Fatal(readBody(r))
	}
	r.Body.Close()
	addr := "127.0.0.1:" + (<-provisioned).Port
	r = f.request(t, "POST", "/api/sites/lab.example.org/invite", nil, f.cookie)
	var invitation struct{ Link string }
	json.NewDecoder(r.Body).Decode(&invitation)
	r.Body.Close()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/socket" {
			c, e := websocket.Accept(w, r, nil)
			if e != nil {
				return
			}
			defer c.CloseNow()
			typ, b, e := c.Read(r.Context())
			if e == nil {
				c.Write(r.Context(), typ, b)
			}
			return
		}
		io.WriteString(w, "Overleaf")
	}))
	defer backend.Close()
	dir := t.TempDir()
	ssh, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer ssh.Close()
	go func() {
		for {
			c, e := ssh.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	cfg, e := InitNode(dir, "127.0.0.1:9899", ssh.Addr().(*net.TCPAddr).Port)
	if e != nil {
		t.Fatal(e)
	}
	n := NewNode(f.ctx, dir, cfg)
	// Pair SSH first; the HTTP forwarding flow must preserve its identity.
	if e = n.Pair(f.invite(t, "SSH")); e != nil {
		t.Fatal(e)
	}
	sshID := n.config.NodeID
	go n.Run()
	waitFor(t, func() bool {
		f.hub.mu.Lock()
		defer f.hub.mu.Unlock()
		l := f.hub.live[sshID]
		return l != nil && l.session != nil && l.status.SSHReady
	})
	forwarded, e := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(n.config.PublicPort)), time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer forwarded.Close()
	verifySSH := func() {
		t.Helper()
		forwarded.SetDeadline(time.Now().Add(time.Second))
		if _, e := io.WriteString(forwarded, "ssh"); e != nil {
			t.Fatal(e)
		}
		b := make([]byte, 3)
		if _, e := io.ReadFull(forwarded, b); e != nil || string(b) != "ssh" {
			t.Fatalf("existing SSH stream failed: %s %v", b, e)
		}
	}
	verifySSH()
	pair := func() int {
		req := httptest.NewRequest("POST", "http://127.0.0.1/api/site", strings.NewReader(string(mustSiteJSON(t, map[string]any{"link": invitation.Link, "port": backend.Listener.Addr().(*net.TCPAddr).Port}))))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-NodeBridge-Request", "1")
		w := httptest.NewRecorder()
		n.Handler().ServeHTTP(w, req)
		return w.Code
	}
	if pair() != 201 || pair() != 400 {
		t.Fatal("one-time pairing failed")
	}
	waitFor(t, func() bool { n.siteMu.Lock(); defer n.siteMu.Unlock(); return n.siteOnline })
	resp, e := http.Get("http://" + addr + "/project")
	if e != nil {
		t.Fatal(e)
	}
	if readBody(resp) != "Overleaf" {
		t.Fatal("upstream mismatch")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, e := websocket.Dial(ctx, "ws://"+addr+"/socket", nil)
	if e != nil {
		t.Fatal(e)
	}
	defer ws.CloseNow()
	ws.Write(ctx, websocket.MessageText, []byte("edit"))
	_, b, e := ws.Read(ctx)
	if e != nil || string(b) != "edit" {
		t.Fatalf("websocket: %s %v", b, e)
	}
	verifySSH()
	r = f.request(t, "DELETE", "/api/sites/lab.example.org/pair", nil, f.cookie)
	r.Body.Close()
	if r.StatusCode != 200 || n.config.NodeID != sshID {
		t.Fatal("revoke affected SSH pairing")
	}
	verifySSH()
}
func mustSiteJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
