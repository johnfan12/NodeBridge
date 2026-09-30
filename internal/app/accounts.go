package app

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"nodebridge/internal/store"
)

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

type accountRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Admin    bool   `json:"admin"`
}

func credentials(username, password string) error {
	if !usernamePattern.MatchString(username) {
		return errors.New("用户名需为 1–64 位字母、数字、点、下划线或连字符")
	}
	if len(password) < 10 || len(password) > 72 {
		return errors.New("密码需为 10–72 字节")
	}
	return nil
}

func (h *Hub) login(w http.ResponseWriter, r *http.Request) {
	if h.limited(r) {
		fail(w, 429, "登录尝试过于频繁，请一分钟后再试")
		return
	}
	var p accountRequest
	if !decode(w, r, &p) {
		return
	}
	var user store.User
	var ok bool
	if err := h.Store.View(func(s store.State) error { user, ok = s.Users[p.Username]; return nil }); err != nil {
		fail(w, 500, "读取账号失败")
		return
	}
	if !ok || bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(p.Password)) != nil {
		h.Store.Update(func(s *store.State) error { s.Record(p.Username, "login.denied", ""); return nil })
		fail(w, 401, "用户名或密码错误")
		return
	}
	token := randomToken()
	err := h.Store.Update(func(s *store.State) error {
		current, exists := s.Users[user.Username]
		if !exists || current.Password != user.Password {
			return errors.New("账号已变更，请重新登录")
		}
		for k, v := range s.Sessions {
			if time.Now().After(v.Expires) {
				delete(s.Sessions, k)
			}
		}
		// Limit concurrent sessions per account to keep the embedded database bounded.
		count := 0
		oldest := ""
		var oldestAt time.Time
		for k, v := range s.Sessions {
			if v.Username == user.Username {
				count++
				if oldest == "" || v.Expires.Before(oldestAt) {
					oldest = k
					oldestAt = v.Expires
				}
			}
		}
		if count >= 10 {
			delete(s.Sessions, oldest)
		}
		s.Sessions[hashToken(token)] = store.Session{Username: user.Username, Expires: time.Now().Add(24 * time.Hour)}
		s.Record(user.Username, "login", "")
		return nil
	})
	if err != nil {
		fail(w, 500, "保存登录失败")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "nodebridge_session", Value: token, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
	user.Password = ""
	respond(w, 200, user)
}

func (h *Hub) logout(w http.ResponseWriter, r *http.Request, u store.User) {
	c, _ := r.Cookie("nodebridge_session")
	if err := h.Store.Update(func(s *store.State) error { delete(s.Sessions, hashToken(c.Value)); return nil }); err != nil {
		fail(w, 500, "退出失败")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "nodebridge_session", Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	respond(w, 200, map[string]bool{"ok": true})
}

func (h *Hub) register(w http.ResponseWriter, r *http.Request) {
	if h.limited(r) {
		fail(w, 429, "请求过于频繁")
		return
	}
	var p accountRequest
	if !decode(w, r, &p) {
		return
	}
	p.Admin = false
	h.addUser(w, p, "register", true)
}

func (h *Hub) createUser(w http.ResponseWriter, r *http.Request, u store.User) {
	var p accountRequest
	if !decode(w, r, &p) {
		return
	}
	h.addUser(w, p, u.Username, false)
}

func (h *Hub) addUser(w http.ResponseWriter, p accountRequest, actor string, registration bool) {
	p.Username = strings.TrimSpace(p.Username)
	if err := credentials(p.Username, p.Password); err != nil {
		fail(w, 400, err.Error())
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(p.Password), bcrypt.DefaultCost)
	if err != nil {
		fail(w, 500, "密码处理失败")
		return
	}
	err = h.Store.Update(func(s *store.State) error {
		if registration && !s.AllowRegister {
			return errors.New("当前未开放自助注册，请联系管理员")
		}
		if _, ok := s.Users[p.Username]; ok {
			return errors.New("用户名已存在")
		}
		s.Users[p.Username] = store.User{Username: p.Username, Password: string(hash), Admin: p.Admin, Created: time.Now().UTC()}
		s.Record(actor, "user.create", p.Username)
		return nil
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	respond(w, 201, map[string]bool{"ok": true})
}

func invalidateSessions(s *store.State, username string) {
	for k, v := range s.Sessions {
		if v.Username == username {
			delete(s.Sessions, k)
		}
	}
}

func (h *Hub) password(w http.ResponseWriter, r *http.Request, u store.User) {
	var p struct {
		Current  string `json:"current"`
		Password string `json:"password"`
	}
	if !decode(w, r, &p) {
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.Password), []byte(p.Current)) != nil {
		fail(w, 400, "当前密码错误")
		return
	}
	if err := credentials(u.Username, p.Password); err != nil {
		fail(w, 400, err.Error())
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(p.Password), bcrypt.DefaultCost)
	if err != nil {
		fail(w, 500, "密码处理失败")
		return
	}
	err = h.Store.Update(func(s *store.State) error {
		v, ok := s.Users[u.Username]
		if !ok {
			return errors.New("账号不存在")
		}
		if v.Password != u.Password {
			return errors.New("账号密码已变更，请重新登录")
		}
		v.Password = string(hash)
		s.Users[u.Username] = v
		invalidateSessions(s, u.Username)
		s.Record(u.Username, "password.change", u.Username)
		return nil
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if u.Username == "admin" {
		os.Remove(filepath.Join(h.dir, "initial-admin.txt"))
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (h *Hub) users(w http.ResponseWriter, r *http.Request, u store.User) {
	users := []store.User{}
	if err := h.Store.View(func(s store.State) error {
		for _, v := range s.Users {
			v.Password = ""
			users = append(users, v)
		}
		return nil
	}); err != nil {
		fail(w, 500, "读取账号失败")
		return
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
	respond(w, 200, users)
}

func adminCount(s *store.State) int {
	count := 0
	for _, u := range s.Users {
		if u.Admin {
			count++
		}
	}
	return count
}

func (h *Hub) editUser(w http.ResponseWriter, r *http.Request, u store.User) {
	username := r.PathValue("username")
	var p struct {
		Admin    *bool  `json:"admin"`
		Password string `json:"password"`
	}
	if !decode(w, r, &p) {
		return
	}
	var hash []byte
	if p.Password != "" {
		if err := credentials(username, p.Password); err != nil {
			fail(w, 400, err.Error())
			return
		}
		var err error
		hash, err = bcrypt.GenerateFromPassword([]byte(p.Password), bcrypt.DefaultCost)
		if err != nil {
			fail(w, 500, "密码处理失败")
			return
		}
	}
	err := h.Store.Update(func(s *store.State) error {
		v, ok := s.Users[username]
		if !ok {
			return errors.New("账号不存在")
		}
		if p.Admin != nil {
			if v.Admin && !*p.Admin && (username == u.Username || adminCount(s) <= 1) {
				return errors.New("不能取消自己或最后一个管理员的权限")
			}
			v.Admin = *p.Admin
		}
		if len(hash) > 0 {
			v.Password = string(hash)
		}
		s.Users[username] = v
		invalidateSessions(s, username)
		s.Record(u.Username, "user.update", username)
		return nil
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if username == "admin" && len(hash) > 0 {
		os.Remove(filepath.Join(h.dir, "initial-admin.txt"))
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (h *Hub) deleteUser(w http.ResponseWriter, r *http.Request, u store.User) {
	username := r.PathValue("username")
	err := h.Store.Update(func(s *store.State) error {
		v, ok := s.Users[username]
		if !ok {
			return errors.New("账号不存在")
		}
		if username == u.Username || (v.Admin && adminCount(s) <= 1) {
			return errors.New("不能删除自己或最后一个管理员")
		}
		delete(s.Users, username)
		invalidateSessions(s, username)
		s.Record(u.Username, "user.delete", username)
		return nil
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}

func (h *Hub) settings(w http.ResponseWriter, r *http.Request, u store.User) {
	var p struct {
		AllowRegister bool `json:"allow_register"`
	}
	if !decode(w, r, &p) {
		return
	}
	if err := h.Store.Update(func(s *store.State) error {
		s.AllowRegister = p.AllowRegister
		s.Record(u.Username, "settings.update", "")
		return nil
	}); err != nil {
		fail(w, 500, "保存设置失败")
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
