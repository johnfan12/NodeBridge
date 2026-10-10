package app

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"nodebridge/internal/store"
)

const maxAnnouncements = 20

var (
	errAnnouncementLimit   = errors.New("最多保留 20 条公告，请先撤下较早的公告")
	errAnnouncementMissing = errors.New("公告不存在或已撤下")
)

func (h *Hub) announcements(w http.ResponseWriter, r *http.Request, u store.User) {
	items := []store.Announcement{}
	if err := h.Store.View(func(s store.State) error {
		// Newest first, using transaction order even if the system clock changes.
		for i := len(s.Announcements) - 1; i >= 0; i-- {
			items = append(items, s.Announcements[i])
		}
		return nil
	}); err != nil {
		fail(w, 500, "读取公告失败")
		return
	}
	respond(w, 200, items)
}

func (h *Hub) publishAnnouncement(w http.ResponseWriter, r *http.Request, u store.User) {
	var p struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if !decode(w, r, &p) {
		return
	}
	p.Title = strings.TrimSpace(p.Title)
	p.Content = strings.TrimSpace(p.Content)
	if p.Title == "" || utf8.RuneCountInString(p.Title) > 100 || strings.ContainsAny(p.Title, "\r\n") {
		fail(w, 400, "公告标题需为 1–100 个字符，不能换行")
		return
	}
	if p.Content == "" || utf8.RuneCountInString(p.Content) > 2000 {
		fail(w, 400, "公告内容需为 1–2000 个字符")
		return
	}
	item := store.Announcement{ID: randomToken(), Title: p.Title, Content: p.Content, Author: u.Username, Created: time.Now().UTC()}
	err := h.Store.Update(func(s *store.State) error {
		if len(s.Announcements) >= maxAnnouncements {
			return errAnnouncementLimit
		}
		s.Announcements = append(s.Announcements, item)
		s.Record(u.Username, "announcement.publish", item.Title)
		return nil
	})
	if errors.Is(err, errAnnouncementLimit) {
		fail(w, 409, err.Error())
		return
	}
	if err != nil {
		fail(w, 500, "保存公告失败")
		return
	}
	respond(w, 201, item)
}

func (h *Hub) withdrawAnnouncement(w http.ResponseWriter, r *http.Request, u store.User) {
	err := h.Store.Update(func(s *store.State) error {
		for i, item := range s.Announcements {
			if item.ID == r.PathValue("id") {
				s.Announcements = append(s.Announcements[:i], s.Announcements[i+1:]...)
				s.Record(u.Username, "announcement.withdraw", item.Title)
				return nil
			}
		}
		return errAnnouncementMissing
	})
	if errors.Is(err, errAnnouncementMissing) {
		fail(w, 404, err.Error())
		return
	}
	if err != nil {
		fail(w, 500, "撤下公告失败")
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
