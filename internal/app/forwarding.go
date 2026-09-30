package app

import (
	"errors"
	"net/http"
	"path/filepath"

	"nodebridge/internal/store"
	"nodebridge/internal/transport"
)

func pausedRequest(w http.ResponseWriter, r *http.Request) (bool, bool) {
	var p struct {
		Paused *bool `json:"paused"`
	}
	if !decode(w, r, &p) {
		return false, false
	}
	if p.Paused == nil {
		fail(w, 400, "请指定 paused: true 或 false")
		return false, false
	}
	return *p.Paused, true
}

func (h *Hub) forwardingStatus(w http.ResponseWriter, r *http.Request, u store.User) {
	h.mu.Lock()
	paused := h.paused
	h.mu.Unlock()
	respond(w, 200, map[string]bool{"paused": paused})
}

func (h *Hub) setForwarding(w http.ResponseWriter, r *http.Request, u store.User) {
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
	err := h.Store.Update(func(s *store.State) error {
		if id == "" {
			s.ForwardingPaused = paused
		} else {
			n, exists := s.Nodes[id]
			if !exists {
				return errors.New("节点不存在")
			}
			n.ForwardingPaused = paused
			s.Nodes[id] = n
		}
		action := "forwarding.resume"
		if paused {
			action = "forwarding.pause"
		}
		target := id
		if target == "" {
			target = "hub"
		}
		s.Record(u.Username, action, target)
		return nil
	})
	if err == nil {
		if id == "" {
			h.paused = paused
		} else if l := h.live[id]; l != nil {
			l.paused = paused
		}
		if paused {
			for nodeID, l := range h.live {
				if id == "" || id == nodeID {
					for conn := range l.connections {
						conn.Close()
					}
				}
			}
		}
	}
	h.mu.Unlock()
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	respond(w, 200, map[string]bool{"paused": paused})
}

func (n *Node) setForwarding(w http.ResponseWriter, r *http.Request) {
	paused, ok := pausedRequest(w, r)
	if !ok {
		return
	}
	n.mu.Lock()
	c := n.config
	c.ForwardingPaused = paused
	if err := WriteJSON(filepath.Join(n.dir, "config.json"), c); err != nil {
		n.mu.Unlock()
		fail(w, 500, "保存暂停状态失败")
		return
	}
	n.config = c
	if paused {
		n.closeStreams(transport.SSH, 0)
	}
	n.mu.Unlock()
	n.notifyStatus()
	respond(w, 200, map[string]bool{"paused": paused})
}
