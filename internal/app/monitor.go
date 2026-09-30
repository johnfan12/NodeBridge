package app

import (
	"log/slog"
	"time"

	"nodebridge/internal/store"
)

func (h *Hub) monitor() {
	defer close(h.monitorDone)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopMonitor:
			return
		case <-h.ctx.Done():
			return
		case now := <-ticker.C:
			if err := h.sampleHealth(now.UTC()); err != nil {
				slog.Error("persist node health", "error", err)
			}
		}
	}
}

func (h *Hub) sampleHealth(now time.Time) error {
	type health struct {
		online, ssh bool
		seen        time.Time
	}
	snapshot := map[string]health{}
	h.mu.Lock()
	for id, l := range h.live {
		online := l.session != nil && !l.session.IsClosed() && !l.seen.IsZero() && now.Sub(l.seen) < 45*time.Second
		snapshot[id] = health{online, online && l.status.SSHReady && l.listener != nil && !h.paused && !l.paused && !l.status.ForwardingPaused, l.seen}
	}
	h.mu.Unlock()
	return h.Store.Update(func(s *store.State) error {
		cutoff := now.AddDate(0, 0, -29).Format("2006-01-02")
		for k, d := range s.Daily {
			if d.Date < cutoff {
				delete(s.Daily, k)
			}
		}
		for id, n := range s.Nodes {
			v := snapshot[id]
			date := now.Format("2006-01-02")
			key := id + "/" + date
			d := s.Daily[key]
			d.NodeID = id
			d.Date = date
			d.Checks++
			if v.online {
				d.Online++
			}
			if v.ssh {
				d.SSHReady++
			}
			s.Daily[key] = d
			if v.seen.After(n.LastSeen) {
				n.LastSeen = v.seen
				s.Nodes[id] = n
			}
		}
		return nil
	})
}
