package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"nodebridge/internal/store"
	"nodebridge/internal/transport"
)

var dnsLabel = regexp.MustCompile("^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$")

func validDomain(d string) bool {
	if len(d) > 253 || !strings.Contains(d, ".") || net.ParseIP(d) != nil {
		return false
	}
	for _, l := range strings.Split(d, ".") {
		if !dnsLabel.MatchString(l) {
			return false
		}
	}
	return true
}
func (h *Hub) siteRoutes(m *http.ServeMux) {
	m.Handle("GET /api/sites", h.auth(true, h.siteList))
	m.Handle("POST /api/sites", h.auth(true, h.siteRegister))
	m.Handle("POST /api/sites/{domain}/invite", h.auth(true, h.siteInvite))
	m.Handle("DELETE /api/sites/{domain}/pair", h.auth(true, h.revokeSite))
	m.HandleFunc("POST /api/site-enroll", h.siteEnroll)
	m.HandleFunc("GET /api/site-connect", h.siteConnect)
}
func (h *Hub) siteList(w http.ResponseWriter, r *http.Request, u store.User) {
	type row struct {
		Port                  int
		Domain                string
		Ready, Online, Paired bool
	}
	rows := []row{}
	h.siteMu.Lock()
	err := h.Store.View(func(st store.State) error {
		for _, s := range st.Sites {
			sess := h.siteSessions[s.Domain]
			online := sess != nil && !sess.IsClosed()
			rows = append(rows, row{s.Port, s.Domain, s.Ready, online, s.CredentialHash != ""})
		}
		return nil
	})
	h.siteMu.Unlock()
	if err != nil {
		fail(w, 500, "读取失败")
		return
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Domain < rows[j].Domain })
	respond(w, 200, rows)
}
func (h *Hub) siteRegister(w http.ResponseWriter, r *http.Request, u store.User) {
	var req struct{ Domain string }
	if !decode(w, r, &req) {
		return
	}
	d := strings.ToLower(strings.TrimSpace(req.Domain))
	public, _ := url.Parse(h.Config.PublicURL)
	if !validDomain(d) || strings.EqualFold(d, public.Hostname()) {
		fail(w, 400, "请使用独立子域名")
		return
	}
	h.siteRegisterMu.Lock()
	defer h.siteRegisterMu.Unlock()
	var site store.Site
	used := map[int]bool{}
	e := h.Store.View(func(st store.State) error {
		site = st.Sites[d]
		for _, s := range st.Sites {
			used[s.Port] = true
		}
		return nil
	})
	if e != nil {
		fail(w, 500, "读取域名失败")
		return
	}
	if site.Port == 0 {
		for p := 41000; p <= 41999; p++ {
			if !used[p] && h.bindSite(d, p) == nil {
				site = store.Site{Domain: d, Port: p}
				break
			}
		}
		if site.Port == 0 {
			fail(w, 503, "回环端口池已满")
			return
		}
		e = h.Store.Update(func(st *store.State) error {
			if st.Sites == nil {
				st.Sites = map[string]store.Site{}
			}
			st.Sites[d] = site
			return nil
		})
		if e != nil {
			h.siteMu.Lock()
			h.siteListeners[d].Close()
			delete(h.siteListeners, d)
			h.siteMu.Unlock()
			fail(w, 500, "保存端口失败")
			return
		}
	} else if e = h.bindSite(d, site.Port); e != nil {
		fail(w, 409, "原回环端口被占用")
		return
	}
	port := strconv.Itoa(site.Port)
	c, e := net.DialTimeout("unix", h.siteSocket, 3*time.Second)
	if e != nil {
		fail(w, 503, "域名管理服务未启用，请在 VPS 运行新版 Hub 安装脚本")
		return
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(4 * time.Minute))
	if e = json.NewEncoder(c).Encode(siteRequest{d, port}); e != nil {
		fail(w, 502, "域名管理服务连接失败")
		return
	}
	var result struct{ Error string }
	if json.NewDecoder(io.LimitReader(c, 8192)).Decode(&result) != nil {
		fail(w, 502, "域名管理服务超时或响应异常")
		return
	}
	if result.Error != "" {
		fail(w, 400, result.Error)
		return
	}
	e = h.Store.Update(func(st *store.State) error {
		if st.Sites == nil {
			st.Sites = map[string]store.Site{}
		}
		s := st.Sites[d]
		s.Domain = d
		s.Ready = true
		st.Sites[d] = s
		st.Record(u.Username, "site.register", d)
		return nil
	})
	if e != nil {
		fail(w, 500, "保存失败，请重试")
		return
	}
	respond(w, 201, true)
}
func (h *Hub) siteInvite(w http.ResponseWriter, r *http.Request, u store.User) {
	d := r.PathValue("domain")
	token := randomToken()
	err := h.Store.Update(func(st *store.State) error {
		s := st.Sites[d]
		if !s.Ready {
			return errors.New("请先注册域名")
		}
		s.InviteHash = hashToken(token)
		s.Expires = time.Now().Add(15 * time.Minute)
		st.Sites[d] = s
		st.Record(u.Username, "site.invite", d)
		return nil
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	link := EncodePair(Pairing{URL: h.Config.PublicURL, Token: token, Fingerprint: h.Fingerprint, TLSMode: TLSAuto})
	respond(w, 201, map[string]string{"link": strings.Replace(link, "//pair/", "//site/", 1)})
}
func (h *Hub) revokeSite(w http.ResponseWriter, r *http.Request, u store.User) {
	d := r.PathValue("domain")
	h.siteMu.Lock()
	defer h.siteMu.Unlock()
	err := h.Store.Update(func(st *store.State) error {
		s, ok := st.Sites[d]
		if !ok {
			return errors.New("未知域名")
		}
		s.CredentialHash = ""
		s.InviteHash = ""
		st.Sites[d] = s
		st.Record(u.Username, "site.revoke", d)
		return nil
	})
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if sess := h.siteSessions[d]; sess != nil {
		sess.Close()
		delete(h.siteSessions, d)
	}
	respond(w, 200, true)
}
func (h *Hub) siteEnroll(w http.ResponseWriter, r *http.Request) {
	var p struct{ Token string }
	if !decode(w, r, &p) {
		return
	}
	cred := randomToken()
	var domain string
	h.siteMu.Lock()
	defer h.siteMu.Unlock()
	err := h.Store.Update(func(st *store.State) error {
		for d, s := range st.Sites {
			if s.InviteHash == hashToken(p.Token) && s.Expires.After(time.Now()) {
				domain = d
				s.InviteHash = ""
				s.CredentialHash = hashToken(cred)
				st.Sites[d] = s
				return nil
			}
		}
		return errors.New("配对链接已失效")
	})
	if err != nil {
		fail(w, 403, err.Error())
		return
	}
	if old := h.siteSessions[domain]; old != nil {
		old.Close()
	}
	respond(w, 201, map[string]string{"domain": domain, "credential": cred})
}
func (h *Hub) siteConnect(w http.ResponseWriter, r *http.Request) {
	d := r.Header.Get("X-Site")
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || len(token) != 43 {
		fail(w, 401, "凭证无效")
		return
	}
	h.siteMu.Lock()
	valid := false
	h.Store.View(func(st store.State) error { valid = st.Sites[d].CredentialHash == hashToken(token); return nil })
	if !valid {
		h.siteMu.Unlock()
		fail(w, 401, "凭证无效")
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		h.siteMu.Unlock()
		return
	}
	conn := websocket.NetConn(h.ctx, ws, websocket.MessageBinary)
	sess, err := yamux.Server(conn, transport.Config())
	if err != nil {
		conn.Close()
		h.siteMu.Unlock()
		return
	}
	if old := h.siteSessions[d]; old != nil {
		old.Close()
	}
	h.siteSessions[d] = sess
	h.siteMu.Unlock()
	defer sess.Close()
	select {
	case <-sess.CloseChan():
	case <-h.ctx.Done():
	}
	h.siteMu.Lock()
	if h.siteSessions[d] == sess {
		delete(h.siteSessions, d)
	}
	h.siteMu.Unlock()
}

func (h *Hub) bindSite(d string, port int) error {
	if port < 41000 || port > 41999 {
		return errors.New("无效回环端口")
	}
	h.siteMu.Lock()
	defer h.siteMu.Unlock()
	if h.siteListeners[d] != nil {
		return nil
	}
	ln, e := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if e != nil {
		return e
	}
	h.siteListeners[d] = ln
	go func() {
		slots := make(chan struct{}, 128)
		for {
			conn, e := ln.Accept()
			if e != nil {
				return
			}
			h.siteMu.Lock()
			sess := h.siteSessions[d]
			h.siteMu.Unlock()
			if sess == nil || sess.IsClosed() {
				conn.Close()
				continue
			}
			select {
			case slots <- struct{}{}:
				go func() {
					defer func() { <-slots }()
					defer conn.Close()
					stream, e := sess.OpenStream()
					if e == nil {
						transport.Bridge(conn, stream)
					}
				}()
			default:
				conn.Close()
			}
		}
	}()
	return nil
}

type siteConfig struct {
	URL, Fingerprint, TLSMode, Domain, Credential string
	Port                                          int
}

func (n *Node) loadSite() (siteConfig, error) {
	var c siteConfig
	b, e := os.ReadFile(filepath.Join(n.dir, "site.json"))
	if e == nil {
		e = json.Unmarshal(b, &c)
	}
	return c, e
}
func (n *Node) siteRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/site", func(w http.ResponseWriter, r *http.Request) {
		n.siteMu.Lock()
		defer n.siteMu.Unlock()
		c, _ := n.loadSite()
		respond(w, 200, map[string]any{"domain": c.Domain, "port": c.Port, "online": n.siteOnline})
	})
	m.HandleFunc("DELETE /api/site", func(w http.ResponseWriter, r *http.Request) {
		n.siteMu.Lock()
		defer n.siteMu.Unlock()
		e := os.Remove(filepath.Join(n.dir, "site.json"))
		if e != nil && !os.IsNotExist(e) {
			fail(w, 500, "清除配对失败")
			return
		}
		if n.siteCancel != nil {
			n.siteCancel()
		}
		n.siteOnline = false
		respond(w, 200, true)
	})
	m.HandleFunc("POST /api/site", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Link string
			Port int
		}
		if !decode(w, r, &req) {
			return
		}
		n.mu.Lock()
		maintenance := n.config.ListenPortForSite()
		n.mu.Unlock()
		if !strings.HasPrefix(req.Link, "nodebridge://site/") || req.Port < 1 || req.Port > 65535 || req.Port == maintenance {
			fail(w, 400, "检查端口与端口转发配对链接")
			return
		}
		p, e := DecodePair(strings.Replace(req.Link, "//site/", "//pair/", 1))
		if e != nil {
			fail(w, 400, e.Error())
			return
		}
		n.siteMu.Lock()
		defer n.siteMu.Unlock()
		client, e := clientForHub(p.URL, p.Fingerprint, p.TLSMode)
		if e != nil {
			fail(w, 400, e.Error())
			return
		}
		defer client.CloseIdleConnections()
		body, _ := json.Marshal(map[string]string{"token": p.Token})
		request, _ := http.NewRequestWithContext(r.Context(), "POST", p.URL+"/api/site-enroll", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-NodeBridge-Request", "1")
		resp, e := client.Do(request)
		if e != nil {
			fail(w, 502, e.Error())
			return
		}
		defer resp.Body.Close()
		var result struct{ Domain, Credential string }
		if resp.StatusCode != 201 || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result) != nil || !validDomain(result.Domain) || len(result.Credential) != 43 {
			fail(w, 400, "配对失败，请重新生成链接")
			return
		}
		c := siteConfig{p.URL, p.Fingerprint, p.TLSMode, result.Domain, result.Credential, req.Port}
		if e = WriteJSON(filepath.Join(n.dir, "site.json"), c); e != nil {
			fail(w, 500, "保存失败，请重新配对")
			return
		}
		n.startSite(c)
		respond(w, 201, true)
	})
}
func (c Config) ListenPortForSite() int {
	_, port, _ := net.SplitHostPort(c.Listen)
	v, _ := strconv.Atoi(port)
	return v
}
func (n *Node) startSite(c siteConfig) {
	if n.siteCancel != nil {
		n.siteCancel()
	}
	ctx, cancel := context.WithCancel(n.ctx)
	n.siteCancel = cancel
	n.siteOnline = false
	go func() {
		for ctx.Err() == nil {
			n.siteTunnel(ctx, c)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
}
func (n *Node) siteTunnel(ctx context.Context, c siteConfig) {
	client, e := clientForHub(c.URL, c.Fingerprint, c.TLSMode)
	if e != nil {
		return
	}
	defer client.CloseIdleConnections()
	ws, _, e := websocket.Dial(ctx, c.URL+"/api/site-connect", &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"X-Site": {c.Domain}, "Authorization": {"Bearer " + c.Credential}}})
	if e != nil {
		return
	}
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	defer conn.Close()
	sess, e := yamux.Client(conn, transport.Config())
	if e != nil {
		return
	}
	defer sess.Close()
	n.siteMu.Lock()
	if ctx.Err() != nil {
		n.siteMu.Unlock()
		return
	}
	n.siteOnline = true
	n.siteMu.Unlock()
	defer func() {
		n.siteMu.Lock()
		if ctx.Err() == nil {
			n.siteOnline = false
		}
		n.siteMu.Unlock()
	}()
	slots := make(chan struct{}, 128)
	for {
		stream, e := sess.AcceptStream()
		if e != nil {
			return
		}
		select {
		case slots <- struct{}{}:
			go func() {
				defer func() { <-slots }()
				defer stream.Close()
				local, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", c.Port), 3*time.Second)
				if e == nil {
					transport.Bridge(stream, local)
				}
			}()
		default:
			stream.Close()
		}
	}
}
