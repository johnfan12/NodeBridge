package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"nodebridge/internal/store"
)

func TestAnnouncementPermissionsAndLifecycle(t *testing.T) {
	base := portPool(t, 1)
	f := newFixture(t, base, base)
	if err := f.hub.Store.Update(func(s *store.State) error {
		s.Users["reader"] = store.User{Username: "reader"}
		s.Sessions[hashToken("reader-token")] = store.Session{Username: "reader", Expires: time.Now().Add(time.Hour)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reader := &http.Cookie{Name: "nodebridge_session", Value: "reader-token"}
	request := func(method, path string, body any, cookie *http.Cookie, want int) string {
		t.Helper()
		r := f.request(t, method, path, body, cookie)
		data := readBody(r)
		if r.StatusCode != want {
			t.Fatalf("%s %s: got %d, want %d: %s", method, path, r.StatusCode, want, data)
		}
		return data
	}
	payload := map[string]string{"title": "  维护通知  ", "content": "  第一行\n<script>alert('test')</script>  "}
	for _, method := range []string{"GET", "POST", "DELETE"} {
		path := "/api/announcements"
		if method == "DELETE" {
			path += "/missing"
		}
		request(method, path, payload, nil, 401)
	}
	request("POST", "/api/announcements", payload, reader, 403)
	request("DELETE", "/api/announcements/missing", nil, reader, 403)
	if got := request("GET", "/api/announcements", nil, reader, 200); strings.TrimSpace(got) != "[]" {
		t.Fatalf("empty board: %s", got)
	}
	for _, invalid := range []map[string]string{
		{"title": "  ", "content": "正文"},
		{"title": "标题", "content": "\n "},
		{"title": strings.Repeat("字", 101), "content": "正文"},
		{"title": "标题", "content": strings.Repeat("字", 2001)},
		{"title": "第一行\n第二行", "content": "正文"},
		{"title": "标题", "content": "正文", "author": "someone-else"},
	} {
		request("POST", "/api/announcements", invalid, f.cookie, 400)
	}
	var first, second store.Announcement
	if err := json.Unmarshal([]byte(request("POST", "/api/announcements", payload, f.cookie, 201)), &first); err != nil {
		t.Fatal(err)
	}
	if first.Title != "维护通知" || first.Content != "第一行\n<script>alert('test')</script>" || first.Author != "admin" || first.ID == "" || first.Created.IsZero() {
		t.Fatalf("published data: %+v", first)
	}
	if err := json.Unmarshal([]byte(request("POST", "/api/announcements", map[string]string{"title": strings.Repeat("字", 100), "content": strings.Repeat("字", 2000)}, f.cookie, 201)), &second); err != nil {
		t.Fatal(err)
	}
	var items []store.Announcement
	if err := json.Unmarshal([]byte(request("GET", "/api/announcements", nil, reader, 200)), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != second.ID || items[1].ID != first.ID {
		t.Fatalf("newest first: %+v", items)
	}
	request("DELETE", "/api/announcements/"+second.ID, nil, f.cookie, 200)
	request("DELETE", "/api/announcements/"+second.ID, nil, f.cookie, 404)
	if err := json.Unmarshal([]byte(request("GET", "/api/announcements", nil, reader, 200)), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != first.ID {
		t.Fatalf("withdrawal removed wrong announcement: %+v", items)
	}
	request("DELETE", "/api/announcements/"+first.ID, nil, f.cookie, 200)
	if got := request("GET", "/api/announcements", nil, reader, 200); strings.TrimSpace(got) != "[]" {
		t.Fatalf("withdrawn board: %s", got)
	}
	if err := f.hub.Store.View(func(s store.State) error {
		var actions []string
		for _, a := range s.Audit {
			if strings.HasPrefix(a.Action, "announcement.") {
				if a.Actor != "admin" || a.Target == "" {
					t.Fatalf("missing actor/target: %+v", a)
				}
				actions = append(actions, a.Action)
			}
		}
		if strings.Join(actions, ",") != "announcement.publish,announcement.publish,announcement.withdraw,announcement.withdraw" {
			t.Fatalf("successful mutations must be audited exactly once: %v", actions)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAnnouncementLimitIsAtomic(t *testing.T) {
	base := portPool(t, 1)
	f := newFixture(t, base, base)
	if err := f.hub.Store.Update(func(s *store.State) error {
		for i := 0; i < maxAnnouncements-1; i++ {
			s.Announcements = append(s.Announcements, store.Announcement{ID: randomToken(), Title: "已有公告", Content: "正文"})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Go(func() {
			r := f.request(t, "POST", "/api/announcements", map[string]string{"title": "新公告", "content": "正文"}, f.cookie)
			r.Body.Close()
			results <- r.StatusCode
		})
	}
	wg.Wait()
	close(results)
	counts := map[int]int{}
	for code := range results {
		counts[code]++
	}
	if counts[201] != 1 || counts[409] != 1 {
		t.Fatalf("concurrent publishes at capacity: %v", counts)
	}
	if err := f.hub.Store.View(func(s store.State) error {
		if len(s.Announcements) != maxAnnouncements {
			t.Fatalf("limit not respected: %d", len(s.Announcements))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
