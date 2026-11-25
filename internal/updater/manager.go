package updater

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"
)

const StateRoot = "/var/lib/nodebridge-updater"

type Request struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type Status struct {
	ID      string    `json:"id,omitempty"`
	Version string    `json:"version,omitempty"`
	Phase   string    `json:"phase,omitempty"`
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at,omitempty"`
}

func Busy(phase string) bool {
	switch phase {
	case "queued", "checking", "downloading", "verifying", "installing", "restarting", "rolling_back":
		return true
	}
	return false
}

func ValidRequest(r Request) bool {
	return regexp.MustCompile(`^[a-zA-Z0-9_-]{43}$`).MatchString(r.ID) && ValidVersion(r.Version)
}

type Manager struct {
	mu        sync.Mutex
	Mode, Dir string
	StateDir  string
	Enabled   func() bool
}

func NewManager(mode, dir string) *Manager {
	m := &Manager{Mode: mode, Dir: dir, StateDir: filepath.Join(StateRoot, mode)}
	m.Enabled = func() bool {
		if filepath.Clean(dir) != "/var/lib/nodebridge/"+mode {
			return false
		}
		_, err := os.Stat("/etc/systemd/system/nodebridge-update-" + mode + ".path")
		if err != nil {
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "/bin/systemctl", "is-active", "--quiet", "nodebridge-update-"+mode+".path").Run() == nil
	}
	return m
}

func (m *Manager) Ready() bool { return m.Enabled != nil && m.Enabled() }

func (m *Manager) Status() Status {
	var s Status
	_ = readJSON(filepath.Join(m.StateDir, "status.json"), &s)
	var r Request
	if readJSON(filepath.Join(m.Dir, "update-request.json"), &r) == nil && ValidRequest(r) && r.ID != s.ID {
		return Status{ID: r.ID, Version: r.Version, Phase: "queued"}
	}
	return s
}

func (m *Manager) Queue(r Request, current string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.Ready() {
		return errors.New("请先用新版安装脚本启用本机更新执行器")
	}
	if !ValidRequest(r) || !Newer(r.Version, current) {
		return errors.New("只允许更新到更高的正式版本")
	}
	if _, err := os.Lstat(filepath.Join(m.Dir, "update-request.json")); err == nil {
		return errors.New("已有升级任务等待执行")
	}
	s := m.Status()
	if Busy(s.Phase) && time.Since(s.At) < 15*time.Minute {
		return errors.New("已有升级任务正在执行")
	}
	return writeJSON(filepath.Join(m.Dir, "update-request.json"), r, 0600)
}

func readJSON(path string, v any) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 {
		return errors.New("更新状态文件无效")
	}
	d := json.NewDecoder(io.LimitReader(f, 16*1024))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("无效 JSON")
	}
	return nil
}

func writeJSON(path string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".update-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func systemctl(args ...string) error { return exec.Command("/bin/systemctl", args...).Run() }
