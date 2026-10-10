package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestRollbackPersistenceAndExclusiveOpen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Update(func(state *State) error {
		state.Nodes["node-a"] = Node{ID: "node-a", Name: "lab", Port: 30001, Created: time.Now()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("rollback")
	err = s.Update(func(state *State) error {
		delete(state.Nodes, "node-a")
		state.Invites["bad"] = Invite{Name: "invalid"}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal("failed update was not returned")
	}
	if other, err := Open(dir); err == nil {
		other.Close()
		t.Fatal("two writers opened the same database")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.View(func(state State) error {
		if state.Nodes["node-a"].Port != 30001 || len(state.Invites) != 0 {
			t.Errorf("rollback/persistence failed: %+v", state)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAnnouncementsOnExistingDatabase(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	legacy := json.RawMessage(`{"old-proxy":{"target_port":8080}}`)
	if err = s.Update(func(state *State) error {
		state.LegacyTCPProxies = legacy
		state.Nodes["node-a"] = Node{ID: "node-a", Port: 30001}
		state.Users["admin"] = User{Username: "admin", Admin: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	item := Announcement{ID: "notice-a", Title: "维护", Content: "今晚维护\n请保存工作", Author: "admin", Created: time.Now().UTC()}
	if err = s.Update(func(state *State) error {
		if len(state.Announcements) != 0 {
			t.Fatal("old database should start with an empty board")
		}
		state.Announcements = append(state.Announcements, item)
		state.Record("admin", "announcement.publish", item.Title)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.View(func(state State) error {
		if len(state.Announcements) != 1 || state.Announcements[0] != item || len(state.Audit) != 1 {
			t.Errorf("announcements/audit did not survive reopening: %+v", state)
		}
		if string(state.LegacyTCPProxies) != string(legacy) || state.Nodes["node-a"].Port != 30001 || !state.Users["admin"].Admin {
			t.Errorf("existing data changed: %+v", state)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
