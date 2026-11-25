package app

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"nodebridge/internal/store"
	"nodebridge/internal/telemetry"
	"nodebridge/internal/transport"
)

func TestGPUSnapshotPersistsAndIgnoresOlderOrLegacyReports(t *testing.T) {
	f := newFixture(t, portPool(t, 1), 65535)
	const id = "snapshot-node"
	if err := f.hub.Store.Update(func(s *store.State) error {
		s.Nodes[id] = store.Node{ID: id, Name: "GPU test", Port: f.hub.Config.PortStart, Created: time.Now()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	server, err := yamux.Server(a, transport.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := yamux.Client(b, transport.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	f.hub.mu.Lock()
	f.hub.live[id] = &liveNode{session: server}
	f.hub.mu.Unlock()
	send := func(status telemetry.Status) {
		t.Helper()
		sent := make(chan error, 1)
		go func() {
			stream, err := client.OpenStream()
			if err == nil {
				stream.SetDeadline(time.Now().Add(3 * time.Second))
				err = json.NewEncoder(stream).Encode(status)
				stream.Close()
			}
			sent <- err
		}()
		stream, err := server.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		f.hub.nodeStatus(id, server, stream)
		stream.Close()
		if err := <-sent; err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Truncate(time.Second)
	status := telemetry.Status{Version: "v0.6.0", Arch: "amd64", GPUSampledAt: now, GPUs: []telemetry.GPU{{Index: "0", Name: "GPU"}}, At: now}
	send(status)
	read := func() *telemetry.GPUSnapshot {
		t.Helper()
		var snapshot *telemetry.GPUSnapshot
		if err := f.hub.Store.View(func(s store.State) error { snapshot = s.Nodes[id].GPUSnapshot; return nil }); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	if got := read(); got == nil || !got.At.Equal(now) || len(got.GPUs) != 1 {
		t.Fatalf("snapshot missing: %+v", got)
	}
	// A duplicate timestamp with different data must not overwrite the cached sample.
	status.GPUs = nil
	status.At = now.Add(10 * time.Second)
	send(status)
	if len(read().GPUs) != 1 {
		t.Fatal("heartbeat replaced persisted snapshot")
	}
	// Old nodes remain compatible and cannot erase a newer snapshot.
	status.GPUSampledAt = time.Time{}
	send(status)
	if got := read(); got == nil || !got.At.Equal(now) {
		t.Fatal("legacy report erased cached snapshot")
	}
	status.GPUSampledAt = now.Add(-time.Hour)
	send(status)
	if !read().At.Equal(now) {
		t.Fatal("out of order report replaced latest snapshot")
	}
	status.GPUSampledAt = now.Add(2 * time.Second)
	status.GPUError = "driver unavailable"
	send(status)
	if read().Error == "" {
		t.Fatal("failed sample did not persist explicit unknown")
	}
	// Simulate Hub having no in-memory session after a restart.
	f.hub.mu.Lock()
	delete(f.hub.live, id)
	f.hub.mu.Unlock()
	r := f.request(t, "GET", "/api/nodes", nil, f.cookie)
	defer r.Body.Close()
	var rows []struct {
		Online   bool                   `json:"online"`
		Snapshot *telemetry.GPUSnapshot `json:"gpu_snapshot"`
	}
	if err := json.NewDecoder(r.Body).Decode(&rows); err != nil || len(rows) != 1 || rows[0].Online || rows[0].Snapshot == nil || rows[0].Snapshot.Error == "" {
		t.Fatalf("offline snapshot not exposed: %+v %v", rows, err)
	}
}
