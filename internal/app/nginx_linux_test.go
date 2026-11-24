package app

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A separate process loads its CA store before any other test can cache system
// roots, exercising the exact trust path used by an installed node.
func TestNginxNodeHelper(t *testing.T) {
	dir := os.Getenv("NODEBRIDGE_NGINX_NODE_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	c, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := NewNode(context.Background(), dir, c)
	link, err := ReadPairFile(filepath.Join(dir, "pair.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err = n.Pair(link); err != nil {
		t.Fatal(err)
	}
	n.Run()
}

func TestNginxPairingForwardingAndCertificateRenewal(t *testing.T) {
	nginx := os.Getenv("NODEBRIDGE_TEST_NGINX")
	if nginx == "" {
		var err error
		nginx, err = exec.LookPath("nginx")
		if err != nil {
			t.Skip("nginx not installed; set NODEBRIDGE_TEST_NGINX to run the real proxy test")
		}
	}
	base := portPool(t, 2)
	f := newFixture(t, base, base+1)
	dir := t.TempDir()
	ca := newTestCA(t)
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	writeCertificate := func() string {
		t.Helper()
		cert := ca.leaf(t, "localhost", false)
		priv, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string][]byte{"proxy.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), "proxy.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv})} {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		return hashBytes(cert.Certificate[0])
	}
	firstFingerprint := writeCertificate()
	front, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := front.Addr().(*net.TCPAddr).Port
	front.Close()
	publicURL := "https://localhost:" + itoa(port)
	c := f.hub.Config
	c.PublicURL = publicURL
	c.Listen = f.server.Listener.Addr().String()
	fragment, err := NginxLocation(c)
	if err != nil {
		t.Fatal(err)
	}
	nginxConfig := fmt.Sprintf(`pid %[1]s/nginx.pid;
error_log %[1]s/nginx.log;
events {}
http {
    access_log off;
    client_body_temp_path %[1]s/body;
    proxy_temp_path %[1]s/proxy;
    fastcgi_temp_path %[1]s/fastcgi;
    uwsgi_temp_path %[1]s/uwsgi;
    scgi_temp_path %[1]s/scgi;
    server {
        listen 127.0.0.1:%[2]d ssl;
        ssl_certificate %[1]s/proxy.pem;
        ssl_certificate_key %[1]s/proxy.key;
        %[3]s
    }
}
`, dir, port, fragment)
	configPath := filepath.Join(dir, "nginx.conf")
	if err := os.WriteFile(configPath, []byte(nginxConfig), 0600); err != nil {
		t.Fatal(err)
	}
	runProcess := func(cmd *exec.Cmd, name string) {
		t.Helper()
		log, err := os.Create(filepath.Join(dir, name+".log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout, cmd.Stderr = log, log
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			log.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			cmd.Wait()
			log.Close()
			if t.Failed() {
				data, _ := os.ReadFile(log.Name())
				t.Log(string(data))
			}
		})
	}
	runProcess(exec.Command(nginx, "-p", dir+"/", "-c", configPath, "-g", "daemon off;"), "proxy-process")
	client, err := clientForHub(publicURL, f.hub.Fingerprint, TLSAuto)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	client.Transport.(*http.Transport).TLSClientConfig.RootCAs = ca.pool
	waitFor(t, func() bool {
		r, err := client.Get(publicURL + "/api/health")
		if err != nil {
			return false
		}
		defer r.Body.Close()
		return r.StatusCode == 200
	})
	// Preserve Origin/Host on browser writes through Nginx, including custom ports.
	f.hub.Config.PublicURL = publicURL
	req, _ := http.NewRequest("POST", publicURL+"/api/invites", strings.NewReader(`{"name":"nginx-node"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-NodeBridge-Request", "1")
	req.Header.Set("Origin", publicURL)
	req.AddCookie(f.cookie)
	r, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var invitation struct {
		Link string `json:"link"`
	}
	if r.StatusCode != 201 || json.NewDecoder(r.Body).Decode(&invitation) != nil {
		r.Body.Close()
		t.Fatal("admin request through Nginx failed")
	}
	r.Body.Close()
	pair, err := DecodePair(invitation.Link)
	if err != nil || pair.TLSMode != TLSAuto || pair.URL != publicURL || pair.Fingerprint == firstFingerprint {
		t.Fatalf("proxy pairing mode/identity incorrect: %v", err)
	}
	ssh, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ssh.Close()
	go func() {
		for {
			conn, err := ssh.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); io.Copy(conn, conn) }()
		}
	}()
	nodeDir := t.TempDir()
	if _, err := InitNode(nodeDir, "127.0.0.1:9899", ssh.Addr().(*net.TCPAddr).Port); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "pair.txt"), []byte(invitation.Link), 0600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestNginxNodeHelper$")
	child.Env = append(os.Environ(), "NODEBRIDGE_NGINX_NODE_DIR="+nodeDir, "SSL_CERT_FILE="+caPath, "SSL_CERT_DIR="+dir+"/empty-ca-directory")
	runProcess(child, "node-process")
	var paired Config
	waitFor(t, func() bool { paired, _ = LoadConfig(nodeDir); return paired.NodeID != "" })
	ready := func() bool {
		f.hub.mu.Lock()
		defer f.hub.mu.Unlock()
		l := f.hub.live[paired.NodeID]
		return l != nil && l.session != nil && !l.session.IsClosed() && !l.seen.IsZero() && l.status.SSHReady
	}
	waitFor(t, ready)
	echo := func() {
		t.Helper()
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", itoa(paired.PublicPort)), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		payload := bytes.Repeat([]byte("SSH through domain proxy\n"), 3000)
		go func() { conn.Write(payload) }()
		data := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, data); err != nil || !bytes.Equal(payload, data) {
			t.Fatalf("SSH forwarding through proxy failed: %v", err)
		}
	}
	echo()
	secondFingerprint := writeCertificate()
	if output, err := exec.Command(nginx, "-p", dir+"/", "-c", configPath, "-s", "reload").CombinedOutput(); err != nil {
		t.Fatalf("certificate reload: %v %s", err, output)
	}
	waitFor(t, func() bool {
		client.CloseIdleConnections()
		r, err := client.Get(publicURL + "/api/health")
		if err != nil {
			return false
		}
		defer r.Body.Close()
		return hashBytes(r.TLS.PeerCertificates[0].Raw) == secondFingerprint
	})
	echo()
	f.hub.mu.Lock()
	old := f.hub.live[paired.NodeID].session
	f.hub.mu.Unlock()
	old.Close() // Force a new TLS handshake against the renewed proxy certificate.
	waitFor(t, func() bool {
		if !ready() {
			return false
		}
		f.hub.mu.Lock()
		defer f.hub.mu.Unlock()
		return f.hub.live[paired.NodeID].session != old
	})
	echo()
	after, err := LoadConfig(nodeDir)
	if err != nil || after.NodeID != paired.NodeID || after.Credential != paired.Credential || after.PublicPort != paired.PublicPort || after.TLSMode != TLSAuto {
		t.Fatal("proxy certificate renewal changed node identity or SSH port")
	}
}
