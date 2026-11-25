package telemetry

import (
	"context"
	"encoding/csv"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"nodebridge/internal/buildinfo"
	"nodebridge/internal/updater"
)

type GPU struct {
	Index       string   `json:"index"`
	Name        string   `json:"name"`
	MemoryTotal *float64 `json:"memory_total_mb"`
	MemoryUsed  *float64 `json:"memory_used_mb"`
	Utilization *float64 `json:"utilization"`
	Temperature *float64 `json:"temperature"`
	Power       *float64 `json:"power_watts"`
}

type Status struct {
	Version          string         `json:"version,omitempty"`
	UpdaterReady     bool           `json:"updater_ready,omitempty"`
	Update           updater.Status `json:"update,omitempty"`
	ForwardingPaused bool           `json:"forwarding_paused,omitempty"`
	Hostname         string         `json:"hostname"`
	OS               string         `json:"os"`
	Arch             string         `json:"arch"`
	CPUs             int            `json:"cpus"`
	SSHReady         bool           `json:"ssh_ready"`
	GPUs             []GPU          `json:"gpus"`
	GPUError         string         `json:"gpu_error,omitempty"`
	GPUSampledAt     time.Time      `json:"gpu_sampled_at,omitempty"`
	At               time.Time      `json:"at"`
}

const GPUInterval = 20 * time.Minute

// GPUSnapshot is the latest sampling result, including failures. Its timestamp
// is independent of the SSH heartbeat, so an old result cannot look live.
type GPUSnapshot struct {
	At    time.Time `json:"at"`
	GPUs  []GPU     `json:"gpus"`
	Error string    `json:"error,omitempty"`
}

func (s Status) Snapshot() *GPUSnapshot {
	if s.GPUSampledAt.IsZero() {
		return nil // Older nodes have no snapshot sampling contract.
	}
	return &GPUSnapshot{At: s.GPUSampledAt, GPUs: s.GPUs, Error: s.GPUError}
}

type Collector struct {
	mu       sync.Mutex
	snapshot GPUSnapshot
	now      func() time.Time
	query    func(context.Context) ([]GPU, error)
}

func NewCollector() *Collector {
	return &Collector{now: time.Now, query: queryGPUs}
}

func (c *Collector) gpuSnapshot(ctx context.Context) GPUSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()
	if ctx.Err() != nil || (!c.snapshot.At.IsZero() && now.Sub(c.snapshot.At) < GPUInterval) {
		return c.snapshot
	}
	gpus, err := c.query(ctx)
	if ctx.Err() != nil {
		return c.snapshot // A reconnect must not cache a canceled query for 20 minutes.
	}
	c.snapshot = GPUSnapshot{At: now, GPUs: gpus}
	if err != nil {
		c.snapshot.GPUs = []GPU{}
		c.snapshot.Error = err.Error()
	}
	return c.snapshot
}

func number(s string) *float64 {
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return nil
	}
	return &n
}

func (collector *Collector) Collect(ctx context.Context, sshPort int) Status {
	host, _ := os.Hostname()
	s := Status{Version: buildinfo.Version, Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), GPUs: []GPU{}, At: time.Now().UTC()}
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(sshPort)), time.Second)
	if err == nil {
		s.SSHReady = true
		c.Close()
	}
	snapshot := collector.gpuSnapshot(ctx)
	s.GPUs, s.GPUError, s.GPUSampledAt = snapshot.GPUs, snapshot.Error, snapshot.At
	return s
}

func queryGPUs(ctx context.Context) ([]GPU, error) {
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, err := exec.CommandContext(qctx, "nvidia-smi", "--query-gpu=index,name,memory.total,memory.used,utilization.gpu,temperature.gpu,power.draw", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, fmt.Errorf("显卡快照采集失败，请检查 NVIDIA 驱动与 nvidia-smi")
	}
	return parseGPUs(string(b))
}

func parseGPUs(data string) ([]GPU, error) {
	rows, err := csv.NewReader(strings.NewReader(data)).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("显卡快照解析失败")
	}
	gpus := []GPU{}
	for _, r := range rows {
		if len(r) != 7 || strings.TrimSpace(r[0]) == "" || strings.TrimSpace(r[1]) == "" {
			return nil, fmt.Errorf("显卡快照字段不完整")
		}
		gpus = append(gpus, GPU{strings.TrimSpace(r[0]), strings.TrimSpace(r[1]), number(r[2]), number(r[3]), number(r[4]), number(r[5]), number(r[6])})
	}
	return gpus, nil
}
