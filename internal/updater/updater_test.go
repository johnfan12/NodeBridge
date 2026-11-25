package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func releaseArchive(t *testing.T, version string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	data := []byte("#!/bin/sh\nprintf 'NodeBridge " + version + "\\n'\n")
	if err := tw.WriteHeader(&tar.Header{Name: "nodebridge", Mode: 0755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func fakeRelease(t *testing.T, data []byte, badHash bool) *http.Client {
	t.Helper()
	h := sha256.Sum256(data)
	digest := fmt.Sprintf("%x", h)
	if badHash {
		digest = strings.Repeat("0", 64)
	}
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "github.com" || !strings.HasPrefix(r.URL.Path, "/johnfan12/NodeBridge/releases/download/v0.6.0/") {
			t.Errorf("unexpected release source: %s", r.URL)
		}
		b := data
		if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
			b = []byte(digest + "  nodebridge-linux-amd64.tar.gz\n" + digest + "  nodebridge-linux-arm64.tar.gz\n")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b)), Header: http.Header{}}, nil
	})}
}

func TestVersionAndTargetAllowlist(t *testing.T) {
	for _, v := range []string{"v0.5.0", "v1.10.3"} {
		if !ValidVersion(v) {
			t.Fatal(v)
		}
	}
	for _, v := range []string{"../v0.5.0", "v1.0.0;id", "v1.0.0-rc1", "v01.0.0", "dev", "v1000000.0.0"} {
		if ValidVersion(v) {
			t.Fatal(v)
		}
	}
	if !Newer("v0.10.0", "v0.9.9") || Newer("v0.5.0", "v0.5.0") || Newer("v0.5.0", "dev") {
		t.Fatal("version ordering")
	}
	for _, s := range []string{"127.0.0.1:443", "github.com:22", "github.com.evil.test:443", "github.com:0443", "api.github.com:80"} {
		if AllowedAddress(s) {
			t.Fatal(s)
		}
	}
	if !AllowedAddress("release-assets.githubusercontent.com:443") {
		t.Fatal("official asset host rejected")
	}
}

func TestDownloadVerifiesBeforeExtraction(t *testing.T) {
	data := releaseArchive(t, "v0.6.0")
	for _, bad := range []bool{false, true} {
		dest := filepath.Join(t.TempDir(), "nodebridge")
		client := fakeRelease(t, data, bad)
		err := Download(context.Background(), client, "v0.6.0", "amd64", dest, func(string) {})
		if bad {
			if err == nil {
				t.Fatal("bad checksum accepted")
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("unverified binary extracted")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			v, err := binaryVersion(context.Background(), dest)
			if err != nil || v != "v0.6.0" {
				t.Fatal(v, err)
			}
		}
	}
}

func TestRedirectAndArchiveSafety(t *testing.T) {
	client := HTTPClient(nil)
	client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://evil.example/file"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	if r, err := client.Get(Repository + "/releases/download/v0.6.0/SHA256SUMS"); err == nil {
		r.Body.Close()
		t.Fatal("untrusted redirect accepted")
	}
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "nodebridge", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	tw.Close()
	gz.Close()
	if err := extractBinary(bytes.NewReader(b.Bytes()), filepath.Join(t.TempDir(), "nodebridge")); err == nil {
		t.Fatal("symlink binary accepted")
	}
}

func TestManagerQueueAndDuplicateProtection(t *testing.T) {
	dir := t.TempDir()
	m := NewManager("node", dir)
	m.StateDir = t.TempDir()
	m.Enabled = func() bool { return true }
	r := Request{ID: strings.Repeat("a", 43), Version: "v0.6.0"}
	if err := m.Queue(r, "v0.5.0"); err != nil {
		t.Fatal(err)
	}
	if s := m.Status(); s.ID != r.ID || s.Phase != "queued" {
		t.Fatalf("queued status: %+v", s)
	}
	if err := m.Queue(r, "v0.5.0"); err == nil {
		t.Fatal("duplicate queue accepted")
	}
	os.Remove(filepath.Join(dir, "update-request.json"))
	if err := m.Queue(Request{ID: r.ID, Version: "v0.4.1"}, "v0.5.0"); err == nil {
		t.Fatal("downgrade accepted")
	}
	m.Enabled = func() bool { return false }
	if err := m.Queue(r, "v0.5.0"); err == nil {
		t.Fatal("unmanaged service accepted update")
	}
}

func TestWorkerSuccessFailureAndRollback(t *testing.T) {
	for _, tc := range []struct {
		name                string
		badHash, failHealth bool
		phase               string
	}{
		{"success", false, false, "succeeded"}, {"checksum failure", true, false, "failed"}, {"startup failure", false, true, "rolled_back"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dataRoot := filepath.Join(root, "data")
			stateRoot := filepath.Join(root, "state")
			target := filepath.Join(root, "nodebridge")
			os.MkdirAll(filepath.Join(dataRoot, "node"), 0700)
			os.WriteFile(target, []byte("#!/bin/sh\nprintf 'NodeBridge v0.5.0\\n'\n"), 0755)
			r := Request{ID: strings.Repeat("a", 43), Version: "v0.6.0"}
			if err := writeJSON(filepath.Join(dataRoot, "node", "update-request.json"), r, 0600); err != nil {
				t.Fatal(err)
			}
			restarts := 0
			w := &Worker{Mode: "node", DataRoot: dataRoot, StateRoot: stateRoot, Target: target, LockPath: filepath.Join(root, "lock"), Client: fakeRelease(t, releaseArchive(t, "v0.6.0"), tc.badHash), Active: func(string) bool { return false }, Restart: func(string) error { restarts++; return nil }}
			w.Health = func(ctx context.Context, mode, version string) error {
				if tc.failHealth && version == "v0.6.0" {
					return fmt.Errorf("test startup failure")
				}
				actual, err := binaryVersion(ctx, target)
				if err != nil || actual != version {
					return fmt.Errorf("wrong live version")
				}
				return nil
			}
			err := w.Run(context.Background())
			if (err == nil) != (tc.phase == "succeeded") {
				t.Fatalf("worker: %v", err)
			}
			var status Status
			if err := readJSON(filepath.Join(stateRoot, "node", "status.json"), &status); err != nil {
				t.Fatal(err)
			}
			if status.Phase != tc.phase {
				t.Fatalf("status: %+v", status)
			}
			version, _ := binaryVersion(context.Background(), target)
			want := "v0.5.0"
			if tc.phase == "succeeded" {
				want = "v0.6.0"
			}
			if version != want {
				t.Fatal(version, want)
			}
			if tc.badHash && restarts != 0 {
				t.Fatal("download failure restarted a live service")
			}
			if tc.failHealth && restarts != 2 {
				t.Fatal("old service was not restarted after rollback")
			}
			if _, err := os.Stat(filepath.Join(dataRoot, "node", "update-request.json")); !os.IsNotExist(err) {
				t.Fatal("consumed request left behind")
			}
		})
	}
}

func TestWorkerRejectsRequestInjection(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "node"), 0700)
	path := filepath.Join(root, "node", "update-request.json")
	os.WriteFile(path, []byte(`{"id":"`+strings.Repeat("a", 43)+`","version":"v0.6.0","command":"touch /tmp/invalid"}`), 0600)
	w := &Worker{Mode: "node", DataRoot: root, StateRoot: t.TempDir(), Target: "unused", LockPath: filepath.Join(root, "lock")}
	if err := w.Run(context.Background()); err == nil {
		t.Fatal("root updater accepted arbitrary fields")
	}
	os.Symlink("/etc/passwd", path)
	if err := w.Run(context.Background()); err == nil {
		t.Fatal("root updater followed a request symlink")
	}
}
