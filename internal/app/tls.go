package app

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const (
	TLSAuto   = "auto"
	TLSPinned = "pinned"
	TLSSystem = "system"
)

func validTLSMode(mode string) bool {
	return mode == "" || mode == TLSAuto || mode == TLSPinned || mode == TLSSystem
}

func verifyPin(cs tls.ConnectionState, fingerprint string) error {
	if len(cs.PeerCertificates) == 0 || hashBytes(cs.PeerCertificates[0].Raw) != fingerprint {
		return errors.New("控制台 TLS 证书指纹不匹配")
	}
	c := cs.PeerCertificates[0]
	if time.Now().Before(c.NotBefore) || time.Now().After(c.NotAfter) {
		return errors.New("控制台 TLS 证书已过期或尚未生效")
	}
	return nil
}

// Empty mode deliberately retains exact pinning for pre-v0.4.0 links/configs.
// New links explicitly select auto: verified Web PKI OR the supplied hub pin.
func hubTLSConfig(hostname, fingerprint, mode string) (*tls.Config, error) {
	if !validTLSMode(mode) {
		return nil, errors.New("不支持的 TLS 校验方式")
	}
	c := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: hostname}
	if mode == TLSSystem {
		return c, nil
	}
	c.InsecureSkipVerify = true // Both supported trust paths are verified explicitly below.
	c.VerifyConnection = func(cs tls.ConnectionState) error {
		if mode == TLSAuto && len(cs.PeerCertificates) > 0 {
			intermediates := x509.NewCertPool()
			for _, cert := range cs.PeerCertificates[1:] {
				intermediates.AddCert(cert)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{DNSName: hostname, Roots: c.RootCAs, Intermediates: intermediates})
			if err == nil {
				return nil
			}
			if pinErr := verifyPin(cs, fingerprint); pinErr == nil {
				return nil
			}
			return fmt.Errorf("控制台 TLS 校验失败：证书既未通过域名/CA 校验，也不匹配配对指纹；请检查 Nginx 证书链和域名，旧节点需更新后使用新链接配对: %w", err)
		}
		return verifyPin(cs, fingerprint)
	}
	return c, nil
}

func clientForHub(rawURL, fingerprint, mode string) (*http.Client, error) {
	u, err := validateURL(rawURL)
	if err != nil {
		return nil, err
	}
	c, err := hubTLSConfig(u.Hostname(), fingerprint, mode)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{TLSClientConfig: c},
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			return errors.New("控制台 API 不应重定向，请检查 public_url 和 Nginx 代理路径")
		},
	}, nil
}

func pinnedClient(fingerprint string) *http.Client {
	c, _ := hubTLSConfig("", fingerprint, TLSPinned)
	return &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: c}}
}
