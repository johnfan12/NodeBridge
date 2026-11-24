package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nodebridge/internal/app"
)

func deploymentCommand(mode string, args []string) error {
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	dir := fs.String("data-dir", "./data/hub", "hub 持久化目录；systemd 安装使用 /var/lib/nodebridge/hub")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("存在无法识别的参数")
	}
	c, err := app.LoadConfig(*dir)
	if err != nil {
		return err
	}
	if mode == "nginx" {
		fragment, err := app.NginxLocation(c)
		if err != nil {
			return err
		}
		fmt.Print(fragment)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.CheckPublicEndpoint(ctx, c); err != nil {
		return err
	}
	fmt.Printf("公网 HTTPS 与 API 路由检查通过：%s\n请使用 nodebridge nginx 生成包含 WebSocket Upgrade 头的反向代理配置。\n", c.PublicURL)
	return nil
}

// Explicit endpoint changes preserve identity, pins, SSH ports, and certificates.
// The old configuration is saved before the atomic replacement, with mode 0600.
func reconfigureEndpoint(dir string, c app.Config, public, hubURL, tlsMode string) (app.Config, error) {
	next := c
	if c.Mode == "hub" {
		if hubURL != "" || tlsMode != "" {
			return c, errors.New("--hub-url 和 --tls-mode 只适用于已配对的 node")
		}
		if public != "" {
			next.PublicURL = strings.TrimRight(public, "/")
		}
	} else {
		if public != "" {
			return c, errors.New("--public-url 只适用于 hub")
		}
		if c.NodeID == "" && (hubURL != "" || tlsMode != "") {
			return c, errors.New("节点尚未配对，请先使用配对链接")
		}
		if hubURL != "" {
			next.HubURL = strings.TrimRight(hubURL, "/")
		}
		if tlsMode != "" {
			next.TLSMode = tlsMode
		}
	}
	if err := next.Validate(); err != nil {
		return c, err
	}
	if next.PublicURL == c.PublicURL && next.HubURL == c.HubURL && next.TLSMode == c.TLSMode {
		return c, nil
	}
	backup := filepath.Join(dir, "config.json.bak-"+time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := app.WriteJSON(backup, c); err != nil {
		return c, err
	}
	if err := app.WriteJSON(filepath.Join(dir, "config.json"), next); err != nil {
		return c, err
	}
	fmt.Fprintln(os.Stderr, "连接地址配置已更新；原配置备份：", backup)
	return next, nil
}
