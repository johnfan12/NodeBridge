package store

import (
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
