package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"nodebridge/internal/telemetry"
	"nodebridge/internal/transport"
	"nodebridge/internal/web"
)

type Node struct {
	mu     sync.Mutex
	config Config
	dir    string
	online bool
	error  string
	port   int
	ctx    context.Context
	wake   chan struct{}
}

func NewNode(ctx context.Context, dir string, c Config) *Node {
	return &Node{config: c, dir: dir, ctx: ctx, wake: make(chan struct{}, 1), port: c.PublicPort}
}

func InitNode(dir, listen string, sshPort int) (Config, error) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return Config{}, err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return Config{}, errors.New("节点维护页面只能监听回环地址，例如 127.0.0.1:9899")
	}
	if sshPort < 1 || sshPort > 65535 {
		return Config{}, errors.New("SSH 端口需为 1–65535")
	}
	c := Config{Mode: "node", Listen: listen, SSHPort: sshPort}
	return c, WriteJSON(filepath.Join(dir, "config.json"), c)
}

func (n *Node) Pair(link string) error {
	p, err := DecodePair(link)
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.config.NodeID != "" {
		return errors.New("节点已配对；重新配对前请先停止服务并移除本地配置")
	}
	body, _ := json.Marshal(map[string]string{"token": p.Token})
	req, err := http.NewRequestWithContext(n.ctx, "POST", p.URL+"/api/enroll", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := pinnedClient(p.Fingerprint).Do(req)
	if err != nil {
		return fmt.Errorf("连接控制台失败: %w", err)
	}
	defer resp.Body.Close()
	var result struct {
		NodeID     string `json:"node_id"`
		Credential string `json:"credential"`
		Port       int    `json:"port"`
		Name       string `json:"name"`
		Error      string `json:"error"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(&result); err != nil {
		return errors.New("控制台响应无效")
	}
	if resp.StatusCode != 201 {
		return fmt.Errorf("配对失败: %s", result.Error)
	}
	if result.NodeID == "" || result.Credential == "" || result.Port < 1 {
		return errors.New("控制台响应缺少节点配置")
	}
	c := n.config
	c.HubURL = p.URL
	c.Fingerprint = p.Fingerprint
	c.NodeID = result.NodeID
	c.Credential = result.Credential
	c.PublicPort = result.Port
	if err = WriteJSON(filepath.Join(n.dir, "config.json"), c); err != nil {
		return fmt.Errorf("保存配置失败，请删除控制台中刚创建的节点并重新配对: %w", err)
	}
	n.config = c
	n.port = result.Port
	n.error = ""
	select {
	case n.wake <- struct{}{}:
	default:
	}
	return nil
}

func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]string{"mode": "node"}) })
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]any{"ok": true, "mode": "node"})
	})
	mux.HandleFunc("GET /api/node", func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		respond(w, 200, map[string]any{"paired": n.config.NodeID != "", "online": n.online, "error": n.error, "hub_url": n.config.HubURL, "node_id": n.config.NodeID, "ssh_port": n.config.SSHPort, "port": n.port})
	})
	mux.HandleFunc("POST /api/pair", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Link string `json:"link"`
		}
		if !decode(w, r, &p) {
			return
		}
		if err := n.Pair(p.Link); err != nil {
			fail(w, 400, err.Error())
			return
		}
		respond(w, 200, map[string]bool{"ok": true})
	})
	mux.Handle("/", web.Handler())
	// Protect the unauthenticated loopback maintenance UI against DNS rebinding.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			fail(w, 403, "请通过 localhost 或回环 IP 访问节点维护页面")
			return
		}
		security(mux).ServeHTTP(w, r)
	})
}

func (n *Node) Run() {
	backoff := time.Second
	for {
		n.mu.Lock()
		c := n.config
		n.mu.Unlock()
		if c.NodeID == "" {
			select {
			case <-n.ctx.Done():
				return
			case <-n.wake:
				continue
			}
		}
		started := time.Now()
		err := n.connect(c)
		n.mu.Lock()
		n.online = false
		if err != nil {
			n.error = err.Error()
		}
		n.mu.Unlock()
		if n.ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Warn("node reconnect", "error", err, "retry_in", backoff)
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		// Add jitter so multiple nodes do not reconnect together after a hub outage.
		delay := backoff + time.Duration(time.Now().UnixNano()%int64(backoff/2+1))
		timer := time.NewTimer(delay)
		select {
		case <-n.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (n *Node) connect(c Config) error {
	client := pinnedClient(c.Fingerprint)
	client.Timeout = 0
	headers := http.Header{}
	headers.Set("X-Node-ID", c.NodeID)
	headers.Set("Authorization", "Bearer "+c.Credential)
	dialCtx, cancel := context.WithTimeout(n.ctx, 15*time.Second)
	ws, resp, err := websocket.Dial(dialCtx, c.HubURL+"/api/connect", &websocket.DialOptions{HTTPClient: client, HTTPHeader: headers})
	cancel()
	if err != nil {
		if resp != nil && resp.StatusCode == 401 {
			return errors.New("节点凭证已撤销，请在控制台重新配对")
		}
		return fmt.Errorf("隧道连接失败: %w", err)
	}
	conn := websocket.NetConn(n.ctx, ws, websocket.MessageBinary)
	session, err := yamux.Client(conn, transport.Config())
	if err != nil {
		conn.Close()
		return err
	}
	defer session.Close()
	n.mu.Lock()
	n.online = true
	n.error = ""
	n.mu.Unlock()
	slog.Info("tunnel connected", "hub", c.HubURL)
	statusCtx, stop := context.WithCancel(n.ctx)
	defer stop()
	go n.report(statusCtx, c, session)
	capacity := make(chan struct{}, 128)
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return err
		}
		select {
		case capacity <- struct{}{}:
			go func() { defer func() { <-capacity }(); n.serveStream(c, stream) }()
		default:
			stream.Close()
		}
	}
}

func (n *Node) report(ctx context.Context, c Config, session *yamux.Session) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		s := telemetry.Collect(ctx, c.SSHPort)
		stream, err := session.OpenStream()
		if err != nil {
			return
		}
		stream.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err = stream.Write([]byte{transport.Status}); err == nil {
			json.NewEncoder(stream).Encode(s)
		}
		stream.Close()
		select {
		case <-ctx.Done():
			return
		case <-session.CloseChan():
			return
		case <-ticker.C:
		}
	}
}

func (n *Node) serveStream(c Config, stream *yamux.Stream) {
	defer stream.Close()
	stream.SetDeadline(time.Now().Add(10 * time.Second))
	var kind [1]byte
	if _, err := io.ReadFull(stream, kind[:]); err != nil || kind[0] != transport.SSH {
		return
	}
	// Hub cannot choose arbitrary destinations: only the configured loopback SSH port.
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(c.SSHPort)), 5*time.Second)
	if err != nil {
		stream.Write([]byte{1})
		return
	}
	defer conn.Close()
	if _, err = stream.Write([]byte{0}); err != nil {
		return
	}
	stream.SetDeadline(time.Time{})
	transport.Bridge(stream, conn)
}

func ReadPairFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(bytes.TrimSpace(b)), err
}
