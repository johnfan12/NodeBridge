package app

import (
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"nodebridge/internal/transport"
)

func (c Config) validateTCPPorts() error {
	if len(c.AllowedTCPPorts) > 16 {
		return errors.New("最多允许 16 个本机 TCP 服务端口")
	}
	_, localPort, _ := net.SplitHostPort(c.Listen)
	maintenance, _ := strconv.Atoi(localPort)
	seen := map[int]bool{}
	for _, port := range c.AllowedTCPPorts {
		if port < 1 || port > 65535 || port == c.SSHPort || port == maintenance || seen[port] {
			return errors.New("本机 TCP 端口需为 1–65535，不可重复或使用 SSH/维护页面端口")
		}
		seen[port] = true
	}
	return nil
}

func (n *Node) notifyStatus() {
	select {
	case n.reportWake <- struct{}{}:
	default:
	}
}

// Must hold n.mu. Limit closure to the selected protocol/port so pausing
// Overleaf never interrupts an unrelated SSH session.
func (n *Node) closeStreams(kind byte, port int) {
	for stream, active := range n.streams {
		if active.kind != kind || (port != 0 && active.port != port) {
			continue
		}
		delete(n.streams, stream)
		stream.SetDeadline(time.Now())
		if active.conn != nil {
			active.conn.Close()
		}
		go stream.Close()
	}
}

func (n *Node) allowTCPPort(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Port int `json:"port"`
	}
	if !decode(w, r, &p) {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	c := n.config
	if !slices.Contains(c.AllowedTCPPorts, p.Port) {
		c.AllowedTCPPorts = append(append([]int{}, c.AllowedTCPPorts...), p.Port)
	}
	if err := c.validateTCPPorts(); err != nil {
		fail(w, 400, err.Error())
		return
	}
	if err := WriteJSON(filepath.Join(n.dir, "config.json"), c); err != nil {
		fail(w, 500, "保存端口授权失败")
		return
	}
	n.config = c
	n.notifyStatus()
	respond(w, 200, map[string]any{"ports": c.AllowedTCPPorts})
}

func (n *Node) removeTCPPort(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		fail(w, 400, "端口无效")
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	c := n.config
	c.AllowedTCPPorts = slices.DeleteFunc(append([]int{}, c.AllowedTCPPorts...), func(p int) bool { return p == port })
	if err := WriteJSON(filepath.Join(n.dir, "config.json"), c); err != nil {
		fail(w, 500, "保存端口授权失败")
		return
	}
	n.config = c
	n.closeStreams(transport.TCP, port)
	n.notifyStatus()
	respond(w, 200, map[string]any{"ports": c.AllowedTCPPorts})
}

func (n *Node) setTCPForwarding(w http.ResponseWriter, r *http.Request) {
	paused, ok := pausedRequest(w, r)
	if !ok {
		return
	}
	n.mu.Lock()
	c := n.config
	c.TCPForwardingPaused = paused
	if err := WriteJSON(filepath.Join(n.dir, "config.json"), c); err != nil {
		n.mu.Unlock()
		fail(w, 500, "保存 TCP 暂停状态失败")
		return
	}
	n.config = c
	if paused {
		n.closeStreams(transport.TCP, 0)
	}
	n.mu.Unlock()
	n.notifyStatus()
	respond(w, 200, map[string]bool{"paused": paused})
}
