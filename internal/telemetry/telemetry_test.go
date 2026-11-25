package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGPUCacheIntervalAndFailureRecovery(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	calls := 0
	failure := false
	c := &Collector{now: func() time.Time { return now }, query: func(context.Context) ([]GPU, error) {
		calls++
		if failure {
			return nil, errors.New("driver unavailable")
		}
		return parseGPUs("0, GPU, 24576, 100, 0, 35, 20")
	}}
	first := c.gpuSnapshot(context.Background())
	for i := 0; i < 120; i++ {
		now = first.At.Add(time.Duration(i) * 10 * time.Second)
		got := c.gpuSnapshot(context.Background())
		if calls != 1 || !got.At.Equal(first.At) || len(got.GPUs) != 1 {
			t.Fatalf("heartbeat queried GPU again: calls=%d snapshot=%+v", calls, got)
		}
	}
	now = first.At.Add(GPUInterval)
	failure = true
	failed := c.gpuSnapshot(context.Background())
	if calls != 2 || failed.Error == "" || len(failed.GPUs) != 0 || !failed.At.Equal(now) {
		t.Fatalf("failure must replace the old reading with explicit unknown: %+v calls=%d", failed, calls)
	}
	now = now.Add(GPUInterval - time.Second)
	failure = false
	if got := c.gpuSnapshot(context.Background()); calls != 2 || got.Error == "" {
		t.Fatalf("failure must also be cached: %+v calls=%d", got, calls)
	}
	now = failed.At.Add(GPUInterval)
	if got := c.gpuSnapshot(context.Background()); calls != 3 || got.Error != "" || len(got.GPUs) != 1 {
		t.Fatalf("next snapshot did not recover: %+v calls=%d", got, calls)
	}
}

func TestGPUCacheSurvivesConcurrentReadersAndCanceledConnection(t *testing.T) {
	var calls atomic.Int32
	c := &Collector{now: time.Now, query: func(context.Context) ([]GPU, error) {
		calls.Add(1)
		return []GPU{}, nil
	}}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.gpuSnapshot(context.Background()) }()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent readers triggered %d GPU queries", calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	c = &Collector{now: time.Now, query: func(context.Context) ([]GPU, error) {
		cancel()
		return nil, ctx.Err()
	}}
	if got := c.gpuSnapshot(ctx); !got.At.IsZero() {
		t.Fatal("disconnected session cached a canceled query")
	}
	c.query = func(context.Context) ([]GPU, error) { return []GPU{}, nil }
	if got := c.gpuSnapshot(context.Background()); got.At.IsZero() || got.Error != "" {
		t.Fatal("new connection could not collect its initial snapshot")
	}
}

func TestSSHReadinessStillChangesInsideGPUCacheWindow(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	defer ln.Close()
	calls := 0
	c := &Collector{now: time.Now, query: func(context.Context) ([]GPU, error) {
		calls++
		return []GPU{}, nil
	}}
	first := c.Collect(context.Background(), port)
	ln.Close()
	second := c.Collect(context.Background(), port)
	if !first.SSHReady || second.SSHReady || calls != 1 || !first.GPUSampledAt.Equal(second.GPUSampledAt) {
		t.Fatalf("SSH health was cached together with GPU data: first=%+v second=%+v calls=%d", first, second, calls)
	}
}

func TestParseGPUUnknownValuesAndMalformedOutput(t *testing.T) {
	gpus, err := parseGPUs("0, NVIDIA Test, 24576, N/A, [Not Supported], 42, 12.5\n1, NVIDIA Test, 24576, 1024, 30, N/A, N/A")
	if err != nil || len(gpus) != 2 || gpus[0].MemoryUsed != nil || gpus[0].Utilization != nil || gpus[1].Temperature != nil {
		t.Fatalf("unknown metrics were treated as zero: %+v %v", gpus, err)
	}
	if _, err := json.Marshal(gpus); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"NaN", "+Inf", "-Inf", "-1", "N/A"} {
		if number(value) != nil {
			t.Errorf("accepted invalid metric %q", value)
		}
	}
	for _, output := range []string{"0, GPU, 42", "0, GPU, 1, 1, 1, 1, 1\n1, GPU, 42", "0, , 1, 1, 1, 1, 1", "\"unterminated"} {
		if _, err := parseGPUs(output); err == nil {
			t.Errorf("accepted incomplete GPU snapshot: %q", output)
		}
	}
}
