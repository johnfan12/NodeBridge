package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"nodebridge/internal/app"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		slog.Error("nodebridge stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage()
		return nil
	}
	mode := os.Args[1]
	if mode == "version" {
		fmt.Println("NodeBridge", version)
		return nil
	}
	if mode == "pair" {
		return pair()
	}
	if mode != "hub" && mode != "node" {
		usage()
		return errors.New("请选择 hub 或 node 模式")
	}
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	data := fs.String("data-dir", "./data/"+mode, "持久化目录")
	listen := fs.String("listen", "", "监听地址；hub 默认 :9443，node 默认 127.0.0.1:9899")
	public := fs.String("public-url", "", "首次启动 hub 必填，例如 https://203.0.113.10:9443")
	start := fs.Int("port-start", 30000, "hub 自动分配端口范围起点")
	end := fs.Int("port-end", 39999, "hub 自动分配端口范围终点")
	ssh := fs.Int("ssh-port", 22, "节点本地 SSH 端口")
	pairFile := fs.String("pair-file", "", "可选：从仅当前用户可读的文件读取配对链接")
	initOnly := fs.Bool("init-only", false, "仅初始化配置和管理员账号，供安装脚本使用")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("存在无法识别的参数")
	}
	dir, err := filepath.Abs(*data)
	if err != nil {
		return err
	}
	c, err := app.LoadConfig(dir)
	if errors.Is(err, os.ErrNotExist) {
		address := *listen
		if mode == "hub" {
			if address == "" {
				address = ":9443"
			}
			c, err = app.InitHub(dir, address, *public, *start, *end)
		} else {
			if address == "" {
				address = "127.0.0.1:9899"
			}
			c, err = app.InitNode(dir, address, *ssh)
		}
	}
	if err != nil {
		return err
	}
	if c.Mode != mode {
		return errors.New("数据目录已经用于另一种模式，请为 hub/node 使用不同目录")
	}
	// Persisted configuration is authoritative. Never silently ignore supplied changes.
	if (*listen != "" && *listen != c.Listen) || (*public != "" && *public != c.PublicURL) {
		return errors.New("参数与已有配置不同，请先停止服务并修改 config.json")
	}
	var mismatch bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port-start":
			mismatch = mismatch || *start != c.PortStart
		case "port-end":
			mismatch = mismatch || *end != c.PortEnd
		case "ssh-port":
			mismatch = mismatch || *ssh != c.SSHPort
		}
	})
	if mismatch {
		return errors.New("参数与已有配置不同，请先停止服务并修改 config.json")
	}
	if *initOnly && mode == "hub" {
		if _, err := os.Stat(filepath.Join(dir, "hub.db")); err == nil {
			return nil
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var server *http.Server
	var hub *app.Hub
	if mode == "hub" {
		hub, err = app.NewHub(ctx, dir, c)
		if err != nil {
			return err
		}
		defer hub.Close()
		if *initOnly {
			return nil
		}
		server = app.HTTPServer(c.Listen, hub.Handler())
		slog.Info("hub ready", "url", c.PublicURL, "initial_credentials", filepath.Join(dir, "initial-admin.txt"), "fingerprint", hub.Fingerprint)
	} else {
		n := app.NewNode(ctx, dir, c)
		if *pairFile != "" {
			link, err := app.ReadPairFile(*pairFile)
			if err != nil {
				return err
			}
			if err = n.Pair(link); err != nil {
				return err
			}
		}
		if *initOnly {
			return nil
		}
		go n.Run()
		server = app.HTTPServer(c.Listen, n.Handler())
		slog.Info("node ready", "local_ui", "http://"+c.Listen)
	}
	done := make(chan error, 1)
	go func() {
		if mode == "hub" {
			done <- server.ListenAndServeTLS(c.CertFile, c.KeyFile)
		} else {
			done <- server.ListenAndServe()
		}
	}()
	select {
	case err := <-done:
		cancel()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err = server.Shutdown(shutdown); err != nil {
			server.Close()
			return err
		}
		return nil
	}
}

func usage() {
	fmt.Println(`NodeBridge — 一个程序，两种模式

nodebridge hub --public-url https://VPS_IP:9443
nodebridge node
nodebridge pair
nodebridge version

hub: HTTPS 控制台、自动配对、SSH 公网端口分配
node: 主动连接 hub，http://127.0.0.1:9899 本地配对页面

使用 nodebridge hub -h 或 nodebridge node -h 查看参数。`)
}

func pair() error {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	address := fs.String("listen", "127.0.0.1:9899", "节点维护地址")
	file := fs.String("file", "", "从文件读取链接；默认粘贴到终端")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("配对只能通过节点本机回环地址进行")
	}
	var link string
	if *file != "" {
		link, err = app.ReadPairFile(*file)
		if err != nil {
			return err
		}
	} else {
		fmt.Print("粘贴控制台生成的配对链接，然后回车：\n")
		scanner := bufio.NewScanner(io.LimitReader(os.Stdin, 8193))
		scanner.Buffer(make([]byte, 1024), 8193)
		if !scanner.Scan() {
			return errors.New("没有读取到配对链接")
		}
		link = strings.TrimSpace(scanner.Text())
	}
	if _, err = app.DecodePair(link); err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"link": link})
	req, err := http.NewRequest("POST", "http://"+*address+"/api/pair", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-NodeBridge-Request", "1")
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("请先启动节点服务: %w", err)
	}
	defer resp.Body.Close()
	var result struct {
		Error string `json:"error"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 16384)).Decode(&result); err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return errors.New(result.Error)
	}
	fmt.Println("配对成功。节点正在自动连接控制台。")
	return nil
}
