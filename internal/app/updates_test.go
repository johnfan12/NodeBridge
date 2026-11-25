package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nodebridge/internal/store"
	"nodebridge/internal/updater"
)

type updateRoundTrip func(*http.Request) (*http.Response, error)

func (f updateRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func enableTestUpdates(m *updater.Manager) { m.Enabled = func() bool { return true } }

func updateFixture(t *testing.T) (*fixture, *Node, string) {
	t.Helper()
	base := portPool(t, 2)
	f := newFixture(t, base, base+1)
	f.hub.version = "v0.5.0"
	enableTestUpdates(f.hub.updates)
	f.hub.updates.StateDir = t.TempDir()
	f.hub.releaseClient = &http.Client{Transport: updateRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.github.com/repos/johnfan12/NodeBridge/releases/latest" {
			t.Errorf("unexpected metadata URL: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"tag_name":"v0.6.0","assets":[{"name":"SHA256SUMS"},{"name":"nodebridge-linux-amd64.tar.gz"},{"name":"nodebridge-linux-arm64.tar.gz"}]}`))}, nil
	})}
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
	dir := t.TempDir()
	config, err := InitNode(dir, "127.0.0.1:9899", ssh.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	n := NewNode(ctx, dir, config)
	n.version = "v0.5.0"
	enableTestUpdates(n.updates)
	n.updates.StateDir = t.TempDir()
	if err := n.Pair(f.invite(t, "更新测试节点")); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { n.Run(); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	id := n.config.NodeID
	waitFor(t, func() bool {
		f.hub.mu.Lock()
		defer f.hub.mu.Unlock()
		l := f.hub.live[id]
		return l != nil && l.status.Version == "v0.5.0" && l.status.UpdaterReady
	})
	return f, n, id
}

func TestAdminUpdateControlAndNodeVersionPersistence(t *testing.T) {
	f, n, id := updateFixture(t)
	if err := f.hub.Store.Update(func(s *store.State) error {
		s.Users["reader"] = store.User{Username: "reader"}
		s.Sessions[hashToken("reader-token")] = store.Session{Username: "reader", Expires: time.Now().Add(time.Hour)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reader := &http.Cookie{Name: "nodebridge_session", Value: "reader-token"}
	for _, route := range []struct{ method, path string }{{"GET", "/api/updates"}, {"POST", "/api/updates/check"}, {"POST", "/api/updates/hub"}, {"POST", "/api/nodes/" + id + "/update"}} {
		for _, cookie := range []*http.Cookie{nil, reader} {
			r := f.request(t, route.method, route.path, map[string]string{"version": "v0.6.0"}, cookie)
			want := 401
			if cookie != nil {
				want = 403
			}
			if r.StatusCode != want {
				t.Fatalf("permission: %s %d", route.path, r.StatusCode)
			}
			r.Body.Close()
		}
	}
	r := f.request(t, "POST", "/api/nodes/"+id+"/update", map[string]string{"version": "v0.6.0"}, f.cookie)
	if r.StatusCode != 400 {
		t.Fatal("unchecked version accepted")
	}
	r.Body.Close()
	r = f.request(t, "POST", "/api/updates/check", map[string]any{}, f.cookie)
	if r.StatusCode != 200 {
		t.Fatal(readBody(r))
	}
	r.Body.Close()
	if _, err := os.Stat(filepath.Join(n.dir, "update-request.json")); !os.IsNotExist(err) {
		t.Fatal("checking versions triggered installation")
	}
	checkPair, err := LoadConfig(n.dir)
	if err != nil {
		t.Fatal(err)
	}
	r = f.request(t, "POST", "/api/nodes/"+id+"/update", map[string]string{"version": "v0.6.0"}, f.cookie)
	if r.StatusCode != 202 {
		t.Fatal(readBody(r))
	}
	r.Body.Close()
	var request updater.Request
	b, err := os.ReadFile(filepath.Join(n.dir, "update-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &request); err != nil || request.Version != "v0.6.0" || request.ID == "" {
		t.Fatal("node did not receive exact update request")
	}
	r = f.request(t, "POST", "/api/nodes/"+id+"/update", map[string]string{"version": "v0.6.0"}, f.cookie)
	if r.StatusCode != 409 {
		t.Fatal("duplicate update accepted")
	}
	r.Body.Close()
	after, err := LoadConfig(n.dir)
	if err != nil || after.NodeID != checkPair.NodeID || after.Credential != checkPair.Credential || after.PublicPort != checkPair.PublicPort {
		t.Fatal("queueing changed node identity")
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(after.PublicPort)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	conn.Write([]byte("still forwarding"))
	data := make([]byte, 16)
	if _, err = io.ReadFull(conn, data); err != nil || string(data) != "still forwarding" {
		t.Fatal("queueing interrupted SSH", err)
	}
	waitFor(t, func() bool {
		var v string
		f.hub.Store.View(func(s store.State) error { v = s.Nodes[id].Version; return nil })
		return v == "v0.5.0"
	})
	r = f.request(t, "GET", "/api/updates", nil, f.cookie)
	body := readBody(r)
	if !strings.Contains(body, "v0.5.0") || !strings.Contains(body, "v0.6.0") || !strings.Contains(body, "queued") {
		t.Fatal(body)
	}
	r = f.request(t, "POST", "/api/updates/hub", map[string]string{"version": "v0.6.0"}, f.cookie)
	if r.StatusCode != 202 {
		t.Fatal(readBody(r))
	}
	r.Body.Close()
	if _, err := os.Stat(filepath.Join(f.dir, "update-request.json")); err != nil {
		t.Fatal("hub request not persisted")
	}
}

func TestReleaseRelayKeepsEndToEndTLSAndRestrictsTargets(t *testing.T) {
	f, n, _ := updateFixture(t)
	ca := newTestCA(t)
	cert := ca.leaf(t, "github.com", false)
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	payload := []byte("#!/bin/sh\nprintf 'NodeBridge v0.6.0\\n'\n")
	tw.WriteHeader(&tar.Header{Name: "nodebridge", Typeflag: tar.TypeReg, Mode: 0755, Size: int64(len(payload))})
	tw.Write(payload)
	tw.Close()
	gz.Close()
	digest := sha256.Sum256(archive.Bytes())
	remote := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
			fmt.Fprintf(w, "%x  nodebridge-linux-amd64.tar.gz\n", digest)
		} else {
			w.Write(archive.Bytes())
		}
	}))
	remote.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	remote.StartTLS()
	defer remote.Close()
	f.hub.releaseDial = func(ctx context.Context, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", remote.Listener.Addr().String())
	}
	local := httptest.NewServer(n.Handler())
	defer local.Close()
	dial, err := updater.RelayDial(local.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if conn, err := dial(context.Background(), "tcp", "127.0.0.1:22"); err == nil {
		conn.Close()
		t.Fatal("arbitrary relay destination accepted")
	}
	client := updater.HTTPClient(dial)
	defer client.CloseIdleConnections()
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: ca.pool}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dest := filepath.Join(t.TempDir(), "nodebridge")
	if err := updater.Download(ctx, client, "v0.6.0", "amd64", dest, func(string) {}); err != nil {
		t.Fatal("relayed HTTPS download failed", err)
	}
	if b, err := os.ReadFile(dest); err != nil || !bytes.Equal(b, payload) {
		t.Fatal("downloaded bytes changed")
	}
	untrusted := updater.HTTPClient(dial)
	defer untrusted.CloseIdleConnections()
	if r, err := untrusted.Get(updater.Repository + "/releases/download/v0.6.0/SHA256SUMS"); err == nil {
		r.Body.Close()
		t.Fatal("relay disabled upstream TLS verification")
	}
}

// Opt-in browser fixture: all update requests stay in temporary directories.
func TestUpdatesBrowserFixture(t *testing.T) {
	output := os.Getenv("NODEBRIDGE_BROWSER_FIXTURE")
	if output == "" {
		t.Skip("browser fixture is opt-in")
	}
	os.Remove(output + ".stop")
	f, n, id := updateFixture(t)
	if err := f.hub.Store.Update(func(s *store.State) error {
		s.Nodes["legacy-node"] = store.Node{ID: "legacy-node", Name: "旧节点（待首次升级）", Port: 30099, Created: time.Now()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	node := httptest.NewServer(n.Handler())
	defer node.Close()
	data := map[string]any{"hub": f.server.URL, "node": node.URL, "node_id": id, "hub_dir": f.dir, "node_dir": n.dir, "node_state_dir": n.updates.StateDir}
	b, _ := json.Marshal(data)
	if err := os.WriteFile(output, b, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	for {
		if _, err := os.Stat(output + ".stop"); err == nil {
			return
		}
		select {
		case <-deadline.C:
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}
