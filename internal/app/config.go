package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	AllowedTCPPorts     []int  `json:"allowed_tcp_ports,omitempty"`
	TCPForwardingPaused bool   `json:"tcp_forwarding_paused,omitempty"`
	ForwardingPaused    bool   `json:"forwarding_paused,omitempty"`
	Mode                string `json:"mode"`
	Listen              string `json:"listen"`
	PublicURL           string `json:"public_url,omitempty"`
	PortStart           int    `json:"port_start,omitempty"`
	PortEnd             int    `json:"port_end,omitempty"`
	HubURL              string `json:"hub_url,omitempty"`
	Fingerprint         string `json:"fingerprint,omitempty"`
	NodeID              string `json:"node_id,omitempty"`
	Credential          string `json:"credential,omitempty"`
	SSHPort             int    `json:"ssh_port,omitempty"`
	PublicPort          int    `json:"public_port,omitempty"`
	CertFile            string `json:"cert_file,omitempty"`
	KeyFile             string `json:"key_file,omitempty"`
}

type Pairing struct {
	URL         string `json:"url"`
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint"`
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func WriteJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func LoadConfig(dir string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	if err != nil {
		return c, err
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("监听地址无效: %w", err)
	}
	switch c.Mode {
	case "hub":
		if _, err = validateURL(c.PublicURL); err != nil {
			return err
		}
		if c.PortStart < 1024 || c.PortEnd > 65535 || c.PortStart > c.PortEnd {
			return errors.New("公网端口池无效")
		}
		if c.CertFile == "" || c.KeyFile == "" {
			return errors.New("缺少 TLS 证书配置")
		}
	case "node":
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("节点维护页面只能监听回环地址")
		}
		if c.SSHPort < 1 || c.SSHPort > 65535 {
			return errors.New("节点 SSH 端口无效")
		}
		if err := c.validateTCPPorts(); err != nil {
			return err
		}
		if c.NodeID != "" {
			if _, err = validateURL(c.HubURL); err != nil {
				return err
			}
			fp, err := hex.DecodeString(c.Fingerprint)
			if err != nil || len(fp) != sha256.Size || len(c.Credential) != 43 || c.PublicPort < 1 || c.PublicPort > 65535 {
				return errors.New("已配对节点配置不完整")
			}
		}
	default:
		return errors.New("配置中 mode 必须为 hub 或 node")
	}
	return nil
}

func validateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("控制台地址必须为 https://IP:端口 或 https://域名")
	}
	return u, nil
}

func EncodePair(p Pairing) string {
	b, _ := json.Marshal(p)
	return "nodebridge://pair/" + base64.RawURLEncoding.EncodeToString(b)
}

func DecodePair(raw string) (Pairing, error) {
	var p Pairing
	if len(raw) > 8192 || !strings.HasPrefix(raw, "nodebridge://pair/") {
		return p, errors.New("无效的配对链接")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, "nodebridge://pair/"))
	if err != nil {
		return p, errors.New("无效的配对链接")
	}
	if err = json.Unmarshal(b, &p); err != nil {
		return p, errors.New("无效的配对链接")
	}
	if _, err = validateURL(p.URL); err != nil {
		return p, err
	}
	fp, err := hex.DecodeString(p.Fingerprint)
	if err != nil || len(fp) != sha256.Size || len(p.Token) != 43 {
		return p, errors.New("配对凭证或证书指纹无效")
	}
	return p, nil
}

// Trust exactly the certificate included by the hub in the pairing link.
// This accepts a self-signed certificate without trusting any other certificate.
func pinnedClient(fingerprint string) *http.Client {
	return &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // Verification is replaced with an exact fingerprint check below.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || hashBytes(cs.PeerCertificates[0].Raw) != fingerprint {
				return errors.New("控制台 TLS 证书指纹不匹配")
			}
			c := cs.PeerCertificates[0]
			if time.Now().Before(c.NotBefore) || time.Now().After(c.NotAfter) {
				return errors.New("控制台 TLS 证书已过期或尚未生效")
			}
			return nil
		},
	}}}
}

func hashBytes(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func certificate(dir, host string) (string, string, string, error) {
	certPath, keyPath := filepath.Join(dir, "tls.pem"), filepath.Join(dir, "tls.key")
	if _, err := os.Stat(certPath); errors.Is(err, os.ErrNotExist) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return "", "", "", err
		}
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			return "", "", "", err
		}
		t := x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "NodeBridge"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(5, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		if ip := net.ParseIP(host); ip != nil {
			t.IPAddresses = []net.IP{ip}
		} else {
			t.DNSNames = []string{host}
		}
		der, err := x509.CreateCertificate(rand.Reader, &t, &t, &key.PublicKey, key)
		if err != nil {
			return "", "", "", err
		}
		priv, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return "", "", "", err
		}
		if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: priv}), 0600); err != nil {
			return "", "", "", err
		}
		if err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
			return "", "", "", err
		}
	}
	c, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return "", "", "", fmt.Errorf("读取 TLS 证书: %w", err)
	}
	return certPath, keyPath, hashBytes(c.Certificate[0]), nil
}
