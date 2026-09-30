package app

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
	"nodebridge/internal/store"
	"nodebridge/internal/transport"
)

type liveProxy struct {
	saved       store.Proxy
	listener    net.Listener
	connections map[net.Conn]struct{}
	error       string
}

// All live proxy access and connection registration is protected by h.mu.
func (p *liveProxy) closeConnections() {
	for conn := range p.connections {
		conn.Close()
	}
}

func (p *liveProxy) close() {
	if p.listener != nil {
		p.listener.Close()
	}
	p.closeConnections()
}

func (h *Hub) restoreProxy(saved store.Proxy) {
	h.mu.Lock()
	p := &liveProxy{saved: saved, connections: map[net.Conn]struct{}{}}
	p.listener, _ = net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(saved.Port)))
	if p.listener == nil {
		p.error = "公网端口被占用或无法监听；释放端口后点击重试监听或恢复转发"
	}
	h.proxies[saved.ID] = p
	h.mu.Unlock()
	if p.listener != nil {
		go h.acceptTCP(saved.ID, p.listener)
	}
}

func (h *Hub) nodeOnline(l *liveNode) bool {
	return l != nil && l.session != nil && !l.session.IsClosed() && !l.seen.IsZero() && time.Since(l.seen) < 45*time.Second
}

func tcpPortStatus(l *liveNode, port int) (allowed, ready bool) {
	if l == nil || !l.status.TCPEnabled {
		return false, false
	}
	for _, p := range l.status.TCPPorts {
		if p.Port == port {
			return true, p.Ready
		}
	}
	return false, false
}

func (h *Hub) listProxies(w http.ResponseWriter, r *http.Request, user store.User) {
	var nodes []store.Node
	if err := h.Store.View(func(s store.State) error {
		for _, n := range s.Nodes {
			nodes = append(nodes, n)
		}
		return nil
	}); err != nil {
		fail(w, 500, "读取代理失败")
		return
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Created.Before(nodes[j].Created) })
	public, _ := url.Parse(h.Config.PublicURL)
	items := []map[string]any{}
	names := map[string]string{}
	h.mu.Lock()
	for _, n := range nodes {
		names[n.ID] = n.Name
		l := h.live[n.ID]
		online := h.nodeOnline(l)
		paused := h.paused || n.ForwardingPaused || (l != nil && l.status.ForwardingPaused)
		item := map[string]any{
			"id": "ssh-" + n.ID, "kind": "ssh", "name": n.Name + " · SSH", "node_id": n.ID, "node_name": n.Name,
			"port": n.Port, "host": public.Hostname(), "scheme": "ssh", "online": online, "allowed": true,
			"paused": n.ForwardingPaused, "effective_paused": paused, "ready": online && !paused && l.listener != nil && l.status.SSHReady,
			"error": "", "target_port": 0,
		}
		if l != nil {
			item["target_port"] = l.status.SSHPort
			item["error"] = l.error
		}
		items = append(items, item)
	}
	saved := make([]*liveProxy, 0, len(h.proxies))
	for _, p := range h.proxies {
		saved = append(saved, p)
	}
	sort.Slice(saved, func(i, j int) bool { return saved[i].saved.Created.Before(saved[j].saved.Created) })
	for _, p := range saved {
		l := h.live[p.saved.NodeID]
		online := h.nodeOnline(l)
		allowed, ready := tcpPortStatus(l, p.saved.TargetPort)
		paused := p.saved.Paused || (l != nil && l.status.TCPPaused)
		items = append(items, map[string]any{
			"id": p.saved.ID, "kind": "tcp", "name": p.saved.Name, "node_id": p.saved.NodeID, "node_name": names[p.saved.NodeID],
			"port": p.saved.Port, "host": public.Hostname(), "scheme": p.saved.Scheme, "target_port": p.saved.TargetPort,
			"online": online, "allowed": allowed, "paused": p.saved.Paused, "effective_paused": paused,
			"ready": online && allowed && ready && !paused && p.listener != nil, "error": p.error,
		})
	}
	h.mu.Unlock()
	respond(w, 200, map[string]any{"items": items, "port_start": h.Config.PortStart, "port_end": h.Config.PortEnd})
}

func (h *Hub) createProxy(w http.ResponseWriter, r *http.Request, user store.User) {
	var request struct {
		Name       string `json:"name"`
		NodeID     string `json:"node_id"`
		TargetPort int    `json:"target_port"`
		Port       int    `json:"port"`
		Scheme     string `json:"scheme"`
	}
	if !decode(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Scheme == "" {
		request.Scheme = "http"
	}
	if request.Name == "" || len(request.Name) > 100 || request.TargetPort < 1 || request.TargetPort > 65535 ||
		(request.Scheme != "http" && request.Scheme != "tcp") {
		fail(w, 400, "请填写有效名称、本机目标端口及 HTTP/TCP 访问方式")
		return
	}
	if request.Port != 0 && (request.Port < h.Config.PortStart || request.Port > h.Config.PortEnd) {
		fail(w, 400, "指定公网端口必须位于配置的端口池内")
		return
	}
	p := store.Proxy{ID: randomToken()[:16], NodeID: request.NodeID, Name: request.Name, TargetPort: request.TargetPort, Scheme: request.Scheme, Created: time.Now().UTC()}
	var listener net.Listener
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		fail(w, 503, "控制台正在停止")
		return
	}
	err := h.Store.Update(func(s *store.State) error {
		if _, ok := s.Nodes[p.NodeID]; !ok {
			return errors.New("节点不存在")
		}
		l := h.live[p.NodeID]
		if !h.nodeOnline(l) || !l.status.TCPEnabled {
			return errors.New("节点需在线并升级到支持 TCP 转发的版本")
		}
		if allowed, _ := tcpPortStatus(l, p.TargetPort); !allowed {
			return errors.New("请先在该节点的本地维护页面授权这个 TCP 端口")
		}
		if len(s.Proxies) >= 100 {
			return errors.New("最多创建 100 个 TCP 代理")
		}
		used := map[int]bool{}
		for _, n := range s.Nodes {
			used[n.Port] = true
		}
		for _, existing := range s.Proxies {
			used[existing.Port] = true
			if existing.NodeID == p.NodeID && existing.TargetPort == p.TargetPort {
				return errors.New("此节点端口已有代理，请使用现有代理")
			}
		}
		start, end := h.Config.PortStart, h.Config.PortEnd
		if request.Port != 0 {
			start, end = request.Port, request.Port
		}
		for candidate := start; candidate <= end; candidate++ {
			if used[candidate] {
				continue
			}
			var err error
			listener, err = net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(candidate)))
			if err == nil {
				p.Port = candidate
				break
			}
		}
		if p.Port == 0 {
			return errors.New("公网端口已占用，或端口池耗尽")
		}
		s.Proxies[p.ID] = p
		s.Record(user.Username, "proxy.create", p.ID)
		return nil
	})
	if err == nil {
		h.proxies[p.ID] = &liveProxy{saved: p, listener: listener, connections: map[net.Conn]struct{}{}}
	}
	h.mu.Unlock()
	if err != nil {
		if listener != nil {
			listener.Close()
		}
		fail(w, 400, err.Error())
		return
	}
	go h.acceptTCP(p.ID, listener)
	respond(w, 201, p)
}

func (h *Hub) pauseProxy(w http.ResponseWriter, r *http.Request, user store.User) {
	paused, ok := pausedRequest(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		fail(w, 503, "控制台正在停止")
		return
	}
	p := h.proxies[id]
	if p == nil {
		h.mu.Unlock()
		fail(w, 404, "TCP 代理不存在；SSH 转发随节点管理")
		return
	}
	updated := p.saved
	updated.Paused = paused
	var newListener net.Listener
	if !paused && p.listener == nil {
		var err error
		newListener, err = net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(updated.Port)))
		if err != nil {
			h.mu.Unlock()
			fail(w, 400, "公网端口仍无法监听，请先释放该端口")
			return
		}
	}
	err := h.Store.Update(func(s *store.State) error {
		s.Proxies[id] = updated
		action := "proxy.resume"
		if paused {
			action = "proxy.pause"
		}
		s.Record(user.Username, action, id)
		return nil
	})
	if err == nil {
		p.saved = updated
		if newListener != nil {
			p.listener = newListener
			p.error = ""
		}
		if paused {
			p.closeConnections()
		}
	}
	h.mu.Unlock()
	if err != nil {
		if newListener != nil {
			newListener.Close()
		}
		fail(w, 500, "保存代理状态失败")
		return
	}
	if newListener != nil {
		go h.acceptTCP(id, newListener)
	}
	respond(w, 200, map[string]bool{"paused": paused})
}

func (h *Hub) deleteProxy(w http.ResponseWriter, r *http.Request, user store.User) {
	id := r.PathValue("id")
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		fail(w, 503, "控制台正在停止")
		return
	}
	err := h.Store.Update(func(s *store.State) error {
		if _, ok := s.Proxies[id]; !ok {
			return errors.New("TCP 代理不存在；SSH 转发随节点管理")
		}
		delete(s.Proxies, id)
		s.Record(user.Username, "proxy.delete", id)
		return nil
	})
	if err == nil {
		if p := h.proxies[id]; p != nil {
			p.close()
		}
		delete(h.proxies, id)
	}
	h.mu.Unlock()
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (h *Hub) acceptTCP(id string, listener net.Listener) {
	capacity := make(chan struct{}, 128)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		select {
		case capacity <- struct{}{}:
			go func() { defer func() { <-capacity }(); h.forwardTCP(id, conn) }()
		default:
			conn.Close()
		}
	}
}

func (h *Hub) forwardTCP(id string, conn net.Conn) {
	defer conn.Close()
	h.mu.Lock()
	p := h.proxies[id]
	var session *yamux.Session
	var targetPort int
	if !h.closed && p != nil && !p.saved.Paused {
		l := h.live[p.saved.NodeID]
		if allowed, ready := tcpPortStatus(l, p.saved.TargetPort); h.nodeOnline(l) && allowed && ready && !l.status.TCPPaused {
			session = l.session
			targetPort = p.saved.TargetPort
			p.connections[conn] = struct{}{}
		}
	}
	h.mu.Unlock()
	if session == nil {
		return
	}
	defer func() { h.mu.Lock(); delete(p.connections, conn); h.mu.Unlock() }()
	stream, err := session.OpenStream()
	if err != nil {
		return
	}
	defer stream.Close()
	stream.SetDeadline(time.Now().Add(10 * time.Second))
	preamble := [3]byte{transport.TCP}
	binary.BigEndian.PutUint16(preamble[1:], uint16(targetPort))
	if _, err := stream.Write(preamble[:]); err != nil {
		return
	}
	var ack [1]byte
	if _, err := io.ReadFull(stream, ack[:]); err != nil || ack[0] != 0 {
		return
	}
	stream.SetDeadline(time.Time{})
	transport.Bridge(conn, stream)
}
