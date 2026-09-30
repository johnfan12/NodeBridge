package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

type User struct {
	Username string    `json:"username"`
	Password string    `json:"password,omitempty"`
	Admin    bool      `json:"admin"`
	Created  time.Time `json:"created"`
}

type Session struct {
	Username string    `json:"username"`
	Expires  time.Time `json:"expires"`
}

type Invite struct {
	Name    string    `json:"name"`
	Expires time.Time `json:"expires"`
}

type Node struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	CredentialHash string    `json:"credential_hash,omitempty"`
	Port           int       `json:"port"`
	Created        time.Time `json:"created"`
	LastSeen       time.Time `json:"last_seen"`
}

type Daily struct {
	NodeID   string `json:"node_id"`
	Date     string `json:"date"`
	Checks   int    `json:"checks"`
	Online   int    `json:"online"`
	SSHReady int    `json:"ssh_ready"`
}

type Audit struct {
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target"`
}

type State struct {
	Version       int                `json:"version"`
	Users         map[string]User    `json:"users"`
	Sessions      map[string]Session `json:"sessions"`
	Invites       map[string]Invite  `json:"invites"`
	Nodes         map[string]Node    `json:"nodes"`
	Audit         []Audit            `json:"audit"`
	AllowRegister bool               `json:"allow_register"`
	Daily         map[string]Daily   `json:"daily"`
}

func (s *State) Record(actor, action, target string) {
	s.Audit = append(s.Audit, Audit{time.Now().UTC(), actor, action, target})
	if len(s.Audit) > 1000 {
		s.Audit = s.Audit[len(s.Audit)-1000:]
	}
}

type Store struct{ db *bolt.DB }

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(filepath.Join(dir, "hub.db"), 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	s := &Store{db}
	if err = s.Update(func(*State) error { return nil }); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func read(tx *bolt.Tx) (State, error) {
	s := State{Version: 1, Users: map[string]User{}, Sessions: map[string]Session{}, Invites: map[string]Invite{}, Nodes: map[string]Node{}, Audit: []Audit{}, Daily: map[string]Daily{}}
	b := tx.Bucket([]byte("state"))
	if b != nil && b.Get([]byte("v1")) != nil {
		if err := json.Unmarshal(b.Get([]byte("v1")), &s); err != nil {
			return s, err
		}
	}
	if s.Version != 1 {
		return s, fmt.Errorf("unsupported database schema version %d", s.Version)
	}
	return s, nil
}

func (s *Store) View(fn func(State) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		v, err := read(tx)
		if err != nil {
			return err
		}
		return fn(v)
	})
}

func (s *Store) Update(fn func(*State) error) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		v, err := read(tx)
		if err != nil {
			return err
		}
		if err = fn(&v); err != nil {
			return err
		}
		b, err := tx.CreateBucketIfNotExists([]byte("state"))
		if err != nil {
			return err
		}
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return b.Put([]byte("v1"), data)
	})
}

func (s *Store) Close() error { return s.db.Close() }
