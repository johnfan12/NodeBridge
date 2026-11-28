package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type siteRequest struct{ Domain, Port string }

// systemd passes a single Unix socket connection on stdin/stdout.
func SiteWorker(in io.Reader, out io.Writer) error {
	var req siteRequest
	d := json.NewDecoder(io.LimitReader(in, 1024))
	d.DisallowUnknownFields()
	err := d.Decode(&req)
	if err == nil {
		err = provisionSite(req)
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return json.NewEncoder(out).Encode(map[string]string{"error": msg})
}
func provisionSite(req siteRequest) error {
	d := req.Domain
	port, err := strconv.Atoi(req.Port)
	if !validDomain(d) || err != nil || port < 41000 || port > 41999 || strconv.Itoa(port) != req.Port {
		return errors.New("域名或分配的回环端口无效")
	}
	lock, err := os.OpenFile("/run/nodebridge-sites.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return errors.New("另一个域名正在注册，请稍后重试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	run := func(cmd string, args ...string) error {
		b, e := exec.CommandContext(ctx, cmd, args...).CombinedOutput()
		if e != nil {
			if len(b) > 1500 {
				b = b[len(b)-1500:]
			}
			return fmt.Errorf("%s: %w %s", filepath.Base(cmd), e, b)
		}
		return nil
	}
	root := "/var/lib/nodebridge-acme"
	file := "/etc/nginx/conf.d/nodebridge-" + d + ".conf"
	httpBlock := fmt.Sprintf("server { listen 80; listen [::]:80; server_name %s; location ^~ /.well-known/acme-challenge/ { root %s; } location / { return 301 https://%s$request_uri; } }\n", d, root, d)
	final := httpBlock + fmt.Sprintf(`server {
 listen 443 ssl;
 listen [::]:443 ssl;
 server_name %[1]s;
 ssl_certificate /etc/letsencrypt/live/%[1]s/fullchain.pem;
 ssl_certificate_key /etc/letsencrypt/live/%[1]s/privkey.pem;
 client_max_body_size 100m;
 location / {
  proxy_pass http://127.0.0.1:%[2]d;
  proxy_http_version 1.1;
  proxy_set_header Host %[1]s;
  proxy_set_header X-Forwarded-Proto https;
  proxy_set_header X-Forwarded-For $remote_addr;
  proxy_set_header Upgrade $http_upgrade;
  proxy_set_header Connection "upgrade";
  proxy_read_timeout 3600s;
  proxy_buffering off;
 }
}
`, d, port)
	old, err := os.ReadFile(file)
	if err == nil && string(old) != httpBlock && string(old) != final {
		return errors.New("同名配置已存在，请检查 Nginx 配置")
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if os.IsNotExist(err) {
		dump, e := exec.CommandContext(ctx, "/usr/sbin/nginx", "-T").Output()
		if e != nil {
			return errors.New("现有 Nginx 配置未通过检查")
		}
		for _, line := range strings.Split(string(dump), "\n") {
			line = strings.SplitN(line, "#", 2)[0]
			for _, word := range strings.Fields(line) {
				if strings.EqualFold(strings.Trim(word, ";\"'"), d) {
					return errors.New("域名已在 Nginx 中配置，请使用独立子域名")
				}
			}
		}
	}
	apply := func(content string) error {
		tmp, e := os.CreateTemp(filepath.Dir(file), ".nodebridge-site-")
		if e != nil {
			return e
		}
		defer os.Remove(tmp.Name())
		_, e = tmp.WriteString(content)
		ce := tmp.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
		if e = os.Rename(tmp.Name(), file); e != nil {
			return e
		}
		check, e := exec.CommandContext(ctx, "/usr/sbin/nginx", "-t").CombinedOutput()
		if e != nil || bytes.Contains(check, []byte("conflicting server name")) {
			return fmt.Errorf("Nginx 检查失败：%s", check)
		}
		loaded, e := exec.CommandContext(ctx, "/usr/sbin/nginx", "-T").Output()
		if e != nil || !bytes.Contains(loaded, []byte("# configuration file "+file+":")) {
			return errors.New("此 Nginx 配置未加载 /etc/nginx/conf.d/*.conf")
		}
		return run("/bin/systemctl", "reload", "nginx")
	}
	if err = os.MkdirAll(root, 0755); err != nil {
		return err
	}
	if string(old) != final {
		if err = apply(httpBlock); err != nil {
			if old == nil {
				os.Remove(file)
			}
			return err
		}
	}
	if err = run("/usr/bin/certbot", "certonly", "--webroot", "-w", root, "--cert-name", d, "-d", d, "--non-interactive", "--agree-tos", "--register-unsafely-without-email", "--keep-until-expiring", "--deploy-hook", "/bin/systemctl reload nginx"); err != nil {
		return err
	}
	if err = apply(final); err != nil {
		if string(old) != final {
			_ = apply(httpBlock)
		}
		return err
	}
	return nil
}
