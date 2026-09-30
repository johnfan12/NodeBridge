package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nodebridge/internal/store"
)

func TestPauseForwardingKeepsManagementAndPairing(t *testing.T) {
	base := portPool(t, 3)
	f := newFixture(t, base, base+2)
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
	nodeDir := t.TempDir()
	c, err := InitNode(nodeDir, "127.0.0.1:9899", ssh.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	n := NewNode(ctx, nodeDir, c)
	if err := n.Pair(f.invite(t, "pause-test")); err != nil {
		t.Fatal(err)
	}
	paired, err := LoadConfig(nodeDir)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { n.Run(); close(done) }()
	defer func() { cancel(); <-done }()
	local := httptest.NewServer(n.Handler())
	defer local.Close()
	waitFor(t, func() bool {
		f.hub.mu.Lock()
		defer f.hub.mu.Unlock()
		l := f.hub.live[paired.NodeID]
		return l.session != nil && !l.seen.IsZero()
	})
	f.hub.mu.Lock()
	session := f.hub.live[paired.NodeID].session
	f.hub.mu.Unlock()
	open := func() net.Conn {
		t.Helper()
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(paired.PublicPort)), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		return conn
	}
	echo := func(conn net.Conn) error {
		if _, err := conn.Write([]byte("test")); err != nil {
			return err
		}
		buf := make([]byte, 4)
		_, err := io.ReadFull(conn, buf)
		if err == nil && string(buf) != "test" {
			t.Fatal("incorrect forwarded payload")
		}
		return err
	}
	toggle := func(path string, paused bool) {
		t.Helper()
		resp := f.request(t, "PUT", path, map[string]bool{"paused": paused}, f.cookie)
		if resp.StatusCode != 200 {
			t.Fatal(readBody(resp))
		}
		resp.Body.Close()
	}
	localToggle := func(paused bool) {
		t.Helper()
		body, _ := json.Marshal(map[string]bool{"paused": paused})
		req, _ := http.NewRequest("PUT", local.URL+"/api/forwarding", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-NodeBridge-Request", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatal(readBody(resp))
		}
		resp.Body.Close()
	}
	nodePath := "/api/nodes/" + paired.NodeID + "/forwarding"
	for _, scope := range []string{"hub", "hub-node", "local"} {
		t.Run(scope, func(t *testing.T) {
			active := open()
			if err := echo(active); err != nil {
				t.Fatal(err)
			}
			path := "/api/forwarding"
			if scope == "hub-node" {
				path = nodePath
			}
			if scope == "local" {
				localToggle(true)
			} else {
				toggle(path, true)
			}
			buf := make([]byte, 1)
			active.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := active.Read(buf); err == nil {
				t.Fatal("active connection survived pause")
			} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("pause did not promptly disconnect active SSH")
			}
			if err := echo(open()); err == nil {
				t.Fatal("new SSH connection allowed while paused")
			}
			if scope == "local" {
				waitFor(t, func() bool {
					f.hub.mu.Lock()
					defer f.hub.mu.Unlock()
					return f.hub.live[paired.NodeID].status.ForwardingPaused
				})
			}
			resp := f.request(t, "GET", "/api/nodes", nil, f.cookie)
			var rows []struct {
				Online   bool `json:"online"`
				SSHReady bool `json:"ssh_ready"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if len(rows) != 1 || !rows[0].Online || rows[0].SSHReady {
				t.Fatalf("management state while paused: %+v", rows)
			}
			loaded, err := LoadConfig(nodeDir)
			if err != nil || loaded.NodeID != paired.NodeID || loaded.Credential != paired.Credential || loaded.PublicPort != paired.PublicPort || loaded.ForwardingPaused != (scope == "local") {
				t.Fatalf("pairing changed or local pause not saved: %v", err)
			}
			if scope == "local" {
				localToggle(false)
				waitFor(t, func() bool {
					f.hub.mu.Lock()
					defer f.hub.mu.Unlock()
					return !f.hub.live[paired.NodeID].status.ForwardingPaused
				})
			} else {
				toggle(path, false)
			}
			if err := echo(open()); err != nil {
				t.Fatalf("resume did not restore forwarding: %v", err)
			}
			f.hub.mu.Lock()
			same := f.hub.live[paired.NodeID].session == session && !session.IsClosed()
			f.hub.mu.Unlock()
			if !same {
				t.Fatal("pause restarted the management connection")
			}
		})
	}
	// Resuming one scope must never bypass a pause in another scope.
	toggle("/api/forwarding", true)
	toggle(nodePath, true)
	toggle("/api/forwarding", false)
	if err := echo(open()); err == nil {
		t.Fatal("hub resume bypassed node pause")
	}
	toggle("/api/forwarding", true)
	if err := f.hub.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewHub(f.ctx, f.dir, f.hub.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if !restarted.paused || !restarted.live[paired.NodeID].paused {
		t.Fatal("hub restart lost pause settings")
	}
}

func TestForwardingAuthorizationAndValidation(t *testing.T) {
	base := portPool(t, 1)
	f := newFixture(t, base, base)
	for _, path := range []string{"/api/forwarding", "/api/nodes/missing/forwarding"} {
		resp := f.request(t, "PUT", path, map[string]bool{"paused": true}, nil)
		if resp.StatusCode != 401 {
			t.Fatal(readBody(resp))
		}
		resp.Body.Close()
	}
	if err := f.hub.Store.Update(func(s *store.State) error {
		s.Users["member"] = store.User{Username: "member"}
		s.Sessions[hashToken("member-token")] = store.Session{Username: "member", Expires: time.Now().Add(time.Hour)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	resp := f.request(t, "PUT", "/api/forwarding", map[string]bool{"paused": true}, &http.Cookie{Name: "nodebridge_session", Value: "member-token"})
	if resp.StatusCode != 403 {
		t.Fatal(readBody(resp))
	}
	resp.Body.Close()
	resp = f.request(t, "PUT", "/api/forwarding", map[string]any{}, f.cookie)
	if resp.StatusCode != 400 {
		t.Fatal(readBody(resp))
	}
	resp.Body.Close()
	resp = f.request(t, "PUT", "/api/nodes/missing/forwarding", map[string]bool{"paused": true}, f.cookie)
	if resp.StatusCode != 400 {
		t.Fatal(readBody(resp))
	}
	resp.Body.Close()
}
