package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"nodebridge/internal/app"
)

func TestReconfigurePreservesHubAndNodeIdentity(t *testing.T) {
	dir := t.TempDir()
	c, err := app.InitHub(dir, ":9443", "https://ssh.example.com:9443", 30000, 30009)
	if err != nil {
		t.Fatal(err)
	}
	c.ForwardingPaused = true
	c.LegacyTCPPorts = []int{8080}
	if err := app.WriteJSON(filepath.Join(dir, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	want := c
	want.PublicURL = "https://ssh.example.com"
	got, err := reconfigureEndpoint(dir, c, want.PublicURL, "", "")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("hub endpoint change: got %+v, err %v", got, err)
	}
	backups, err := filepath.Glob(filepath.Join(dir, "config.json.bak-*"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backup missing: %v %v", backups, err)
	}
	stat, err := os.Stat(backups[0])
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatalf("backup permissions: %v %v", stat, err)
	}
	var node = app.Config{Mode: "node", Listen: "127.0.0.1:9899", SSHPort: 22, HubURL: "https://ssh.example.com:9443", NodeID: "node-a", Credential: "1234567890123456789012345678901234567890123", Fingerprint: "1234567890123456789012345678901234567890123456789012345678901234", PublicPort: 30005, ForwardingPaused: true}
	nodeDir := t.TempDir()
	if err := app.WriteJSON(filepath.Join(nodeDir, "config.json"), node); err != nil {
		t.Fatal(err)
	}
	want = node
	want.HubURL = "https://ssh.example.com"
	want.TLSMode = app.TLSAuto
	got, err = reconfigureEndpoint(nodeDir, node, "", want.HubURL, app.TLSAuto)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("node identity/port changed: got %+v, err %v", got, err)
	}
	loaded, err := app.LoadConfig(nodeDir)
	if err != nil || !reflect.DeepEqual(loaded, want) {
		t.Fatalf("endpoint update was not persisted: %+v %v", loaded, err)
	}
	for _, tc := range []struct{ public, hubURL, mode string }{
		{"", "http://ssh.example.com", app.TLSAuto},
		{"", "https://ssh.example.com", "insecure"},
		{"https://ssh.example.com", "", ""},
	} {
		if _, err := reconfigureEndpoint(nodeDir, loaded, tc.public, tc.hubURL, tc.mode); err == nil {
			t.Fatal("invalid endpoint change accepted")
		}
		after, err := app.LoadConfig(nodeDir)
		if err != nil || !reflect.DeepEqual(after, loaded) {
			t.Fatal("failed change modified the configuration")
		}
	}
}
