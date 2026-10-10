package app

import (
	"encoding/json"
	"net"
	"nodebridge/internal/store"
	"path/filepath"
	"testing"
)

func TestSSHOnlyUpgradePreservesLegacyTCPMetadata(t *testing.T) {
	base := portPool(t, 2)
	f := newFixture(t, base, base+1)
	legacy := json.RawMessage(`{"old-overleaf":{"id":"old-overleaf","node_id":"old-node","name":"Overleaf","port":` + itoa(base+1) + `,"target_port":8080}}`)
	if err := f.hub.Store.Update(func(s *store.State) error { s.LegacyTCPProxies = legacy; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Close(); err != nil {
		t.Fatal(err)
	}
	h, err := NewHub(f.ctx, f.dir, f.hub.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	// Retired TCP records must not reactivate public listeners after an upgrade.
	listener, err := net.Listen("tcp", ":"+itoa(base+1))
	if err != nil {
		t.Fatal("legacy TCP port still bound", err)
	}
	listener.Close()
	if err := h.Store.View(func(s store.State) error {
		if string(s.LegacyTCPProxies) != string(legacy) {
			t.Fatal("legacy migration records lost")
		}
		if !s.Users["admin"].Admin {
			t.Fatal("existing administrator lost")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/proxies", "/api/tcp-ports", "/api/tcp-forwarding"} {
		r := f.request(t, "GET", path, nil, f.cookie)
		must := r.StatusCode
		r.Body.Close()
		if must != 404 {
			t.Fatalf("retired API %s: %d", path, must)
		}
	}
	dir := t.TempDir()
	c, err := InitNode(dir, "127.0.0.1:9899", 22)
	if err != nil {
		t.Fatal(err)
	}
	c.LegacyTCPPorts = []int{8080}
	c.LegacyTCPPaused = true
	if err := WriteJSON(filepath.Join(dir, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	c, err = LoadConfig(dir)
	if err != nil || len(c.LegacyTCPPorts) != 1 || c.LegacyTCPPorts[0] != 8080 || !c.LegacyTCPPaused {
		t.Fatal("legacy local migration settings lost", err)
	}
}
