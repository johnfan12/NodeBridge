package app

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"nodebridge/internal/store"
	"nodebridge/internal/telemetry"
	"nodebridge/internal/transport"
	"nodebridge/internal/updater"
)

func (h *Hub) checkUpdate(w http.ResponseWriter, r *http.Request, u store.User) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	release, err := updater.Latest(ctx, h.releaseClient)
	if err != nil {
		fail(w, 502, "检查更新失败："+err.Error())
		return
	}
	if err = WriteJSON(filepath.Join(h.dir, "latest-release.json"), release); err != nil {
		fail(w, 500, "保存版本信息失败")
		return
	}
	h.updateMu.Lock()
	h.latest = release
	h.updateMu.Unlock()
	respond(w, 200, release)
}

func (h *Hub) updateOverview(w http.ResponseWriter, r *http.Request, u store.User) {
	var nodes []store.Node
	var tasks map[string]updater.Status
	if err := h.Store.View(func(s store.State) error {
		for _, n := range s.Nodes {
			nodes = append(nodes, n)
		}
		tasks = s.UpdateTasks
		return nil
	}); err != nil {
		fail(w, 500, "读取版本状态失败")
		return
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	h.updateMu.Lock()
	latest := h.latest
	h.updateMu.Unlock()
	hubStatus := h.updates.Status()
	if hubStatus.ID == "" {
		hubStatus = tasks["hub"]
	}
	if old, ok := tasks["hub"]; ok && old.ID == hubStatus.ID && old.Phase != hubStatus.Phase {
		_ = h.Store.Update(func(s *store.State) error {
			s.UpdateTasks["hub"] = hubStatus
			if !updater.Busy(hubStatus.Phase) {
				s.Record("system", "update."+hubStatus.Phase, "hub "+hubStatus.Version)
			}
			return nil
		})
	}
	rows := []map[string]any{}
	h.mu.Lock()
	for _, n := range nodes {
		version, arch := n.Version, n.Arch
		ready, online := false, false
		job := tasks[n.ID]
		if l := h.live[n.ID]; l != nil {
			online = l.session != nil && !l.session.IsClosed() && !l.seen.IsZero() && time.Since(l.seen) < 45*time.Second
			if online {
				version = l.status.Version
				arch = l.status.Arch
				ready = l.status.UpdaterReady
				if l.status.Update.ID != "" && (job.ID == "" || job.ID == l.status.Update.ID) {
					job = l.status.Update
				}
			}
		}
		rows = append(rows, map[string]any{"id": n.ID, "name": n.Name, "version": version, "arch": arch, "online": online, "updater_ready": ready, "update": job})
	}
	h.mu.Unlock()
	respond(w, 200, map[string]any{"latest": latest, "hub": map[string]any{"version": h.version, "arch": runtime.GOARCH, "updater_ready": h.updates.Ready(), "update": hubStatus}, "nodes": rows})
}

func (h *Hub) startUpdate(w http.ResponseWriter, r *http.Request, u store.User) {
	var p struct {
		Version string `json:"version"`
	}
	if !decode(w, r, &p) {
		return
	}
	h.updateMu.Lock()
	latest := h.latest
	h.updateMu.Unlock()
	if !updater.ValidVersion(p.Version) || p.Version != latest.Version || latest.CheckedAt.IsZero() {
		fail(w, 400, "请先检查更新，再选择检测到的正式版本")
		return
	}
	id := r.PathValue("id")
	target := id
	if target == "" {
		target = "hub"
	}
	request := updater.Request{ID: randomToken(), Version: p.Version}
	// Persist an audit intent before handing off to an independent root service.
	if err := h.Store.Update(func(s *store.State) error {
		if id != "" {
			if _, ok := s.Nodes[id]; !ok {
				return errors.New("节点不存在")
			}
		}
		if s.UpdateTasks == nil {
			s.UpdateTasks = map[string]updater.Status{}
		}
		if old := s.UpdateTasks[target]; updater.Busy(old.Phase) && time.Since(old.At) < 15*time.Minute {
			return errors.New("目标已有升级任务，请等待完成")
		}
		s.UpdateTasks[target] = updater.Status{ID: request.ID, Version: p.Version, Phase: "queued", At: time.Now().UTC()}
		s.Record(u.Username, "update.request", target+" "+p.Version)
		return nil
	}); err != nil {
		fail(w, 409, err.Error())
		return
	}
	var err error
	if id == "" {
		err = h.updates.Queue(request, h.version)
	} else {
		err = h.sendNodeUpdate(id, request)
	}
	if err != nil {
		_ = h.Store.Update(func(s *store.State) error {
			v := s.UpdateTasks[target]
			if v.ID == request.ID {
				v.Phase = "failed"
				v.Message = err.Error()
				s.UpdateTasks[target] = v
			}
			s.Record(u.Username, "update.rejected", target)
			return nil
		})
		fail(w, 409, err.Error())
		return
	}
	respond(w, 202, request)
}

func (h *Hub) sendNodeUpdate(id string, request updater.Request) error {
	h.mu.Lock()
	l := h.live[id]
	var session *yamux.Session
	if l != nil && l.session != nil && !l.session.IsClosed() && time.Since(l.seen) < 45*time.Second && l.status.UpdaterReady && updater.Newer(request.Version, l.status.Version) {
		session = l.session
	}
	h.mu.Unlock()
	if session == nil {
		return errors.New("节点离线、版本过旧，或尚未安装更新执行器")
	}
	s, err := session.OpenStream()
	if err != nil {
		return err
	}
	defer s.Close()
	s.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err = s.Write([]byte{transport.Update}); err != nil {
		return err
	}
	if err = json.NewEncoder(s).Encode(request); err != nil {
		return err
	}
	var ack struct {
		Accepted bool   `json:"accepted"`
		Error    string `json:"error"`
	}
	if err = json.NewDecoder(io.LimitReader(s, 4096)).Decode(&ack); err != nil {
		return err
	}
	if !ack.Accepted {
		return errors.New(ack.Error)
	}
	return nil
}

func (n *Node) receiveUpdate(s *yamux.Stream) {
	var request updater.Request
	if err := json.NewDecoder(io.LimitReader(s, 4096)).Decode(&request); err != nil {
		return
	}
	err := n.updates.Queue(request, n.version)
	result := map[string]any{"accepted": err == nil}
	if err != nil {
		result["error"] = err.Error()
	}
	_ = json.NewEncoder(s).Encode(result)
	select {
	case n.reportWake <- struct{}{}:
	default:
	}
}

func (h *Hub) nodeStatus(id string, session *yamux.Session, s *yamux.Stream) {
	var status telemetry.Status
	if err := json.NewDecoder(io.LimitReader(s, 64*1024)).Decode(&status); err != nil {
		return
	}
	if len(status.Version) > 64 || len(status.Arch) > 20 {
		return
	}
	snapshot := status.Snapshot()
	if snapshot != nil && (snapshot.At.After(time.Now().Add(time.Minute)) || len(snapshot.GPUs) > 128 || len(snapshot.Error) > 1024) {
		return
	}
	h.mu.Lock()
	valid := false
	if l := h.live[id]; l != nil && l.session == session {
		l.status = status
		l.seen = time.Now().UTC()
		valid = true
	}
	h.mu.Unlock()
	if !valid {
		return
	}
	needsSave := false
	_ = h.Store.View(func(state store.State) error {
		n, ok := state.Nodes[id]
		if !ok {
			return nil
		}
		needsSave = status.Version != "" && (n.Version != status.Version || n.Arch != status.Arch)
		if snapshot != nil && (n.GPUSnapshot == nil || snapshot.At.After(n.GPUSnapshot.At)) {
			needsSave = true
		}
		if task, ok := state.UpdateTasks[id]; ok && task.ID == status.Update.ID && task.Phase != status.Update.Phase {
			needsSave = true
		}
		return nil
	})
	if !needsSave {
		return
	}
	_ = h.Store.Update(func(state *store.State) error {
		n, ok := state.Nodes[id]
		if !ok {
			return nil
		}
		if status.Version != "" && (n.Version != status.Version || n.Arch != status.Arch) {
			n.Version = status.Version
			n.Arch = status.Arch
		}
		if snapshot != nil && (n.GPUSnapshot == nil || snapshot.At.After(n.GPUSnapshot.At)) {
			n.GPUSnapshot = snapshot
		}
		state.Nodes[id] = n
		if task, ok := state.UpdateTasks[id]; ok && task.ID == status.Update.ID && task.Phase != status.Update.Phase {
			state.UpdateTasks[id] = status.Update
			if !updater.Busy(status.Update.Phase) {
				state.Record("node", "update."+status.Update.Phase, id+" "+task.Version)
			}
		}
		return nil
	})
}

func (h *Hub) releaseStream(s *yamux.Stream) {
	var size [2]byte
	if _, err := io.ReadFull(s, size[:]); err != nil {
		return
	}
	n := binary.BigEndian.Uint16(size[:])
	if n == 0 || n > 255 {
		return
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(s, b); err != nil {
		return
	}
	address := string(b)
	if !updater.AllowedAddress(address) {
		return
	}
	s.SetDeadline(time.Now().Add(15 * time.Second))
	select {
	case h.releaseSlots <- struct{}{}:
		defer func() { <-h.releaseSlots }()
	default:
		return
	}
	var conn net.Conn
	var err error
	if h.releaseDial != nil {
		conn, err = h.releaseDial(h.ctx, address)
	} else {
		conn, err = net.DialTimeout("tcp", address, 10*time.Second)
	}
	if err != nil {
		return
	}
	defer conn.Close()
	if _, err = s.Write([]byte{0}); err != nil {
		return
	}
	s.SetDeadline(time.Time{})
	transport.Bridge(s, conn)
}

func (n *Node) updateRelay(w http.ResponseWriter, r *http.Request) {
	address := r.URL.Query().Get("address")
	if r.Header.Get("Origin") != "" || !updater.AllowedAddress(address) {
		fail(w, 403, "更新中转目标不允许")
		return
	}
	n.mu.Lock()
	session := n.session
	n.mu.Unlock()
	if session == nil {
		fail(w, 503, "节点尚未连接控制台")
		return
	}
	s, err := session.OpenStream()
	if err != nil {
		fail(w, 503, "中转连接失败")
		return
	}
	defer s.Close()
	s.SetDeadline(time.Now().Add(15 * time.Second))
	packet := make([]byte, 3+len(address))
	packet[0] = transport.ReleaseDial
	binary.BigEndian.PutUint16(packet[1:3], uint16(len(address)))
	copy(packet[3:], address)
	if _, err = s.Write(packet); err != nil {
		fail(w, 502, "中转请求失败")
		return
	}
	var ack [1]byte
	if _, err = io.ReadFull(s, ack[:]); err != nil || ack[0] != 0 {
		fail(w, 502, "VPS 无法连接更新服务器")
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	s.SetDeadline(time.Time{})
	conn := websocket.NetConn(n.ctx, ws, websocket.MessageBinary)
	defer conn.Close()
	transport.Bridge(s, conn)
}
