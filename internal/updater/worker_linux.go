package updater

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type localConfig struct {
	Mode     string `json:"mode"`
	Listen   string `json:"listen"`
	CertFile string `json:"cert_file"`
}

type Worker struct {
	Mode, DataRoot, StateRoot, Target, LockPath string
	Client                                      *http.Client
	Restart                                     func(string) error
	Active                                      func(string) bool
	Health                                      func(context.Context, string, string) error
	RequireRoot                                 bool
}

func NewWorker(mode string) *Worker {
	w := &Worker{Mode: mode, DataRoot: "/var/lib/nodebridge", StateRoot: StateRoot, Target: "/usr/local/bin/nodebridge", LockPath: "/run/nodebridge-update.lock", RequireRoot: true}
	w.Restart = func(mode string) error { return systemctl("restart", "nodebridge-"+mode+".service") }
	w.Active = func(mode string) bool { return systemctl("is-active", "--quiet", "nodebridge-"+mode+".service") == nil }
	w.Health = w.health
	return w
}

func (w *Worker) Run(ctx context.Context) (result error) {
	if (w.Mode != "hub" && w.Mode != "node") || (w.RequireRoot && os.Geteuid() != 0) {
		return errors.New("更新执行器只允许 root 运行固定的 hub/node 模式")
	}
	if w.RequireRoot {
		for _, path := range []string{w.DataRoot, w.StateRoot, filepath.Dir(w.Target)} {
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || stat.Uid != 0 || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
				return errors.New("更新器的父目录必须由 root 持有且不可被其他用户写入")
			}
		}
		info, err := os.Lstat(filepath.Join(w.DataRoot, w.Mode))
		if err != nil || !info.IsDir() {
			return errors.New("更新数据目录无效")
		}
		info, err = os.Lstat(w.Target)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
			return errors.New("程序必须由 root 持有且不可被其他用户写入")
		}
	}
	lock, err := os.OpenFile(w.LockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	requestPath := filepath.Join(w.DataRoot, w.Mode, "update-request.json")
	f, err := os.OpenFile(requestPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		f.Close()
		return errors.New("升级请求文件无效")
	}
	var request Request
	d := json.NewDecoder(io.LimitReader(f, 4096))
	d.DisallowUnknownFields()
	err = d.Decode(&request)
	var extra any
	trailing := d.Decode(&extra)
	f.Close()
	if err != nil || trailing != io.EOF || !ValidRequest(request) {
		os.Remove(requestPath)
		return errors.New("升级请求无效")
	}
	os.Remove(requestPath)
	stateDir := filepath.Join(w.StateRoot, w.Mode)
	if err = os.MkdirAll(stateDir, 0755); err != nil {
		return err
	}
	status := Status{ID: request.ID, Version: request.Version}
	stage := func(phase string) {
		status.Phase = phase
		status.At = time.Now().UTC()
		slog.Info("update stage", "mode", w.Mode, "version", request.Version, "phase", phase)
		_ = writeJSON(filepath.Join(stateDir, "status.json"), status, 0644)
	}
	defer func() {
		if result != nil {
			status.Message = result.Error()
			if status.Phase != "rolled_back" {
				status.Phase = "failed"
			}
			status.At = time.Now().UTC()
			_ = writeJSON(filepath.Join(stateDir, "status.json"), status, 0644)
		}
	}()
	stage("checking")
	current, err := binaryVersion(ctx, w.Target)
	if err != nil {
		return err
	}
	if !Newer(request.Version, current) {
		return errors.New("只允许安装高于当前二进制的正式版本")
	}
	if w.Client == nil {
		var dial func(context.Context, string, string) (net.Conn, error)
		if w.Mode == "node" {
			c, err := w.config(w.Mode)
			if err != nil {
				return err
			}
			dial, err = RelayDial(c.Listen)
			if err != nil {
				return err
			}
		}
		w.Client = HTTPClient(dial)
	}
	defer w.Client.CloseIdleConnections()
	private := filepath.Join(stateDir, "private")
	if err = os.MkdirAll(private, 0700); err != nil {
		return err
	}
	work, err := os.MkdirTemp(private, "job-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	candidate := filepath.Join(work, "nodebridge")
	if err = Download(ctx, w.Client, request.Version, runtime.GOARCH, candidate, stage); err != nil {
		return err
	}
	v, err := binaryVersion(ctx, candidate)
	if err != nil || v != request.Version {
		return errors.New("安装包二进制版本不匹配")
	}
	backup := filepath.Join(private, "previous-nodebridge")
	if err = copyBinary(w.Target, backup); err != nil {
		return fmt.Errorf("备份二进制失败: %w", err)
	}
	modes := []string{w.Mode}
	other := "hub"
	if w.Mode == "hub" {
		other = "node"
	}
	if w.Active(other) {
		modes = append(modes, other)
	}
	stage("installing")
	if err = copyBinary(candidate, w.Target); err != nil {
		return err
	}
	stage("restarting")
	for _, mode := range modes {
		if err = w.Restart(mode); err == nil {
			err = w.Health(ctx, mode, request.Version)
		}
		if err != nil {
			break
		}
	}
	if err != nil {
		cause := err
		stage("rolling_back")
		if rollbackErr := copyBinary(backup, w.Target); rollbackErr != nil {
			return fmt.Errorf("新版启动失败: %v；恢复旧二进制失败: %w", cause, rollbackErr)
		}
		for _, mode := range modes {
			if restartErr := w.Restart(mode); restartErr != nil {
				return fmt.Errorf("已恢复旧二进制，但服务恢复失败: %w", restartErr)
			}
			if healthErr := w.Health(ctx, mode, current); healthErr != nil {
				return fmt.Errorf("已恢复旧二进制，但旧服务未通过启动检查: %w", healthErr)
			}
		}
		stage("rolled_back")
		return fmt.Errorf("新版启动检查失败，已恢复 %s: %w", current, cause)
	}
	stage("succeeded")
	return nil
}

func binaryVersion(ctx context.Context, path string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	b, err := exec.CommandContext(c, path, "version").Output()
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(b))
	if len(f) != 2 || f[0] != "NodeBridge" || !ValidVersion(f[1]) {
		return "", errors.New("当前程序不是正式发布版本")
	}
	return f[1], nil
}

func copyBinary(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	f, err := os.CreateTemp(filepath.Dir(destination), ".nodebridge-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0755); err == nil {
		_, err = io.Copy(f, in)
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
	return os.Rename(f.Name(), destination)
}

func (w *Worker) config(mode string) (localConfig, error) {
	var c localConfig
	f, err := os.OpenFile(filepath.Join(w.DataRoot, mode, "config.json"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return c, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 {
		return c, errors.New("服务配置文件无效")
	}
	err = json.NewDecoder(io.LimitReader(f, 16384)).Decode(&c)
	if c.Mode != mode {
		return c, errors.New("服务配置模式不匹配")
	}
	return c, err
}

func (w *Worker) health(ctx context.Context, mode, version string) error {
	c, err := w.config(mode)
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		host = "127.0.0.1"
	}
	endpoint := "http://" + net.JoinHostPort(host, port) + "/api/health"
	t := &http.Transport{}
	if mode == "hub" {
		file, err := os.OpenFile(c.CertFile, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			file.Close()
			return errors.New("Hub 证书文件无效")
		}
		data, err := io.ReadAll(io.LimitReader(file, 256*1024))
		file.Close()
		if err != nil {
			return err
		}
		block, _ := pem.Decode(data)
		if block == nil {
			return errors.New("Hub 证书无效")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return err
		}
		t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !bytes.Equal(cs.PeerCertificates[0].Raw, cert.Raw) {
				return errors.New("本机 Hub 证书不匹配")
			}
			return nil
		}}
		endpoint = "https://" + net.JoinHostPort(host, port) + "/api/health"
	}
	client := &http.Client{Transport: t, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("健康检查不接受重定向") }}
	defer client.CloseIdleConnections()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
		r, err := client.Do(req)
		if err == nil {
			var health struct {
				OK      bool   `json:"ok"`
				Mode    string `json:"mode"`
				Version string `json:"version"`
			}
			err = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&health)
			r.Body.Close()
			if err == nil && r.StatusCode == 200 && health.OK && health.Mode == mode && health.Version == version {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("服务未在 30 秒内启动到目标版本")
		case <-time.After(500 * time.Millisecond):
		}
	}
}
