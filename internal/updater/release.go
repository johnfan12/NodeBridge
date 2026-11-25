package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const Repository = "https://github.com/johnfan12/NodeBridge"

var versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})$`)

func ValidVersion(v string) bool { return versionPattern.MatchString(v) }

func Newer(a, b string) bool {
	if !ValidVersion(a) || !ValidVersion(b) {
		return false
	}
	x, y := strings.Split(a[1:], "."), strings.Split(b[1:], ".")
	for i := range x {
		xn, _ := strconv.Atoi(x[i])
		yn, _ := strconv.Atoi(y[i])
		if xn != yn {
			return xn > yn
		}
	}
	return false
}

func AllowedHost(host string) bool {
	switch host {
	case "github.com", "api.github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com", "github-releases.githubusercontent.com":
		return true
	}
	return false
}

func AllowedAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	return err == nil && port == "443" && AllowedHost(host)
}

func HTTPClient(dial func(context.Context, string, string) (net.Conn, error)) *http.Client {
	t := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: dial, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 30 * time.Second, MaxConnsPerHost: 2}
	if dial != nil {
		t.Proxy = nil
	} // The node relay supplies only an opaque connection; normal TLS verification still runs.
	return &http.Client{Transport: t, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 || r.URL.Scheme != "https" || !AllowedHost(r.URL.Hostname()) || (r.URL.Port() != "" && r.URL.Port() != "443") {
			return errors.New("更新下载重定向目标不在允许列表")
		}
		return nil
	}}
}

type Release struct {
	Version   string    `json:"version"`
	URL       string    `json:"url"`
	CheckedAt time.Time `json:"checked_at"`
}

func Latest(ctx context.Context, client *http.Client) (Release, error) {
	b, err := fetch(ctx, client, "https://api.github.com/repos/johnfan12/NodeBridge/releases/latest", 1024*1024)
	if err != nil {
		return Release{}, err
	}
	var v struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
		} `json:"assets"`
	}
	if err = json.Unmarshal(b, &v); err != nil || !ValidVersion(v.Tag) || v.Draft || v.Prerelease {
		return Release{}, errors.New("未找到有效的正式发布版本")
	}
	names := map[string]bool{}
	for _, a := range v.Assets {
		names[a.Name] = true
	}
	if !names["SHA256SUMS"] || !names["nodebridge-linux-amd64.tar.gz"] || !names["nodebridge-linux-arm64.tar.gz"] {
		return Release{}, errors.New("发布文件尚未齐全，请稍后检查")
	}
	return Release{v.Tag, Repository + "/releases/tag/" + v.Tag, time.Now().UTC()}, nil
}

func fetch(ctx context.Context, client *http.Client, rawURL string, limit int64) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || !AllowedHost(u.Hostname()) {
		return nil, errors.New("更新来源不允许")
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "NodeBridge-updater")
		r, err := client.Do(req)
		if err == nil {
			var b []byte
			if r.StatusCode == 200 {
				b, err = io.ReadAll(io.LimitReader(r.Body, limit+1))
				if err == nil && int64(len(b)) > limit {
					err = errors.New("更新文件超过大小限制")
				}
			} else {
				err = fmt.Errorf("更新服务器返回 HTTP %d", r.StatusCode)
			}
			r.Body.Close()
			if err == nil {
				return b, nil
			}
			if r.StatusCode >= 400 && r.StatusCode < 500 && r.StatusCode != 429 {
				return nil, err
			}
		}
		last = err
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second * time.Duration(attempt+1)):
			}
		}
	}
	return nil, fmt.Errorf("下载失败，已尝试三次: %w", last)
}

// Both the manifest and archive are obtained independently over verified HTTPS
// from the fixed repository. No hash, URL, or executable is accepted from a job.
func Download(ctx context.Context, client *http.Client, version, arch, destination string, stage func(string)) error {
	if !ValidVersion(version) || (arch != "amd64" && arch != "arm64") {
		return errors.New("版本或架构不支持")
	}
	base := Repository + "/releases/download/" + version + "/"
	stage("checking")
	sums, err := fetch(ctx, client, base+"SHA256SUMS", 64*1024)
	if err != nil {
		return err
	}
	asset := "nodebridge-linux-" + arch + ".tar.gz"
	expected := ""
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			if expected != "" {
				return errors.New("重复的发布校验记录")
			}
			expected = fields[0]
		}
	}
	digest, err := hex.DecodeString(expected)
	if err != nil || len(digest) != sha256.Size {
		return errors.New("发布校验和缺失或无效")
	}
	stage("downloading")
	data, err := fetch(ctx, client, base+asset, 128*1024*1024)
	if err != nil {
		return err
	}
	stage("verifying")
	h := sha256.Sum256(data)
	if hex.EncodeToString(h[:]) != expected {
		return errors.New("安装包 SHA-256 不匹配，已停止更新")
	}
	return extractBinary(bytes.NewReader(data), destination)
}

func extractBinary(reader io.Reader, destination string) error {
	gz, err := gzip.NewReader(reader)
	if err != nil {
		return err
	}
	defer gz.Close()
	t := tar.NewReader(io.LimitReader(gz, 256*1024*1024))
	found := false
	for {
		header, err := t.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if header.Name != "nodebridge" {
			continue
		}
		if found || header.Typeflag != tar.TypeReg || header.Size < 1 || header.Size > 128*1024*1024 {
			return errors.New("发布二进制格式无效")
		}
		found = true
		f, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(f, t)
		syncErr := f.Sync()
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if !found {
		return errors.New("安装包缺少 nodebridge")
	}
	return nil
}
