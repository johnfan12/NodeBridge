package telemetry

import (
	"context"
	"encoding/csv"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
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
	Hostname string    `json:"hostname"`
	OS       string    `json:"os"`
	Arch     string    `json:"arch"`
	CPUs     int       `json:"cpus"`
	SSHReady bool      `json:"ssh_ready"`
	GPUs     []GPU     `json:"gpus"`
	GPUError string    `json:"gpu_error,omitempty"`
	At       time.Time `json:"at"`
}

func number(s string) *float64 {
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return nil
	}
	return &n
}

func Collect(ctx context.Context, sshPort int) Status {
	host, _ := os.Hostname()
	s := Status{Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), GPUs: []GPU{}, At: time.Now().UTC()}
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(sshPort)), time.Second)
	if err == nil {
		s.SSHReady = true
		c.Close()
	}
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, err := exec.CommandContext(qctx, "nvidia-smi", "--query-gpu=index,name,memory.total,memory.used,utilization.gpu,temperature.gpu,power.draw", "--format=csv,noheader,nounits").Output()
	if err != nil {
		s.GPUError = "未检测到可用的 nvidia-smi"
		return s
	}
	rows, err := csv.NewReader(strings.NewReader(string(b))).ReadAll()
	if err != nil {
		s.GPUError = "GPU 状态解析失败"
		return s
	}
	for _, r := range rows {
		if len(r) != 7 {
			continue
		}
		s.GPUs = append(s.GPUs, GPU{strings.TrimSpace(r[0]), strings.TrimSpace(r[1]), number(r[2]), number(r[3]), number(r[4]), number(r[5]), number(r[6])})
	}
	return s
}
