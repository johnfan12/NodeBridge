package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "NodeBridge test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return testCA{cert, key, pool}
}

func (ca testCA) leaf(t *testing.T, host string, expired bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: serial, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	if expired {
		template.NotAfter = time.Now().Add(-time.Minute)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: priv}))
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestHubTLSModesAndCertificateRenewal(t *testing.T) {
	ca := newTestCA(t)
	otherCA := newTestCA(t)
	first := ca.leaf(t, "localhost", false)
	second := ca.leaf(t, "localhost", false)
	wrongHost := ca.leaf(t, "wrong.example", false)
	expired := ca.leaf(t, "localhost", true)
	untrusted := otherCA.leaf(t, "localhost", false)
	var active atomic.Pointer[tls.Certificate]
	active.Store(&first)
	var requests atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(204) }))
	srv.TLS = &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return active.Load(), nil }}
	srv.StartTLS()
	srv.URL = strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	defer srv.Close()
	for _, tc := range []struct {
		name, mode string
		cert       *tls.Certificate
		pin        string
		accepted   bool
	}{
		{"proxy CA certificate differs from Hub", TLSAuto, &first, strings.Repeat("0", 64), true},
		{"renewed proxy CA certificate", TLSAuto, &second, hashBytes(first.Certificate[0]), true},
		{"pinned untrusted certificate", TLSAuto, &untrusted, hashBytes(untrusted.Certificate[0]), true},
		{"untrusted unrelated certificate", TLSAuto, &untrusted, hashBytes(first.Certificate[0]), false},
		{"wrong domain rejected", TLSAuto, &wrongHost, hashBytes(first.Certificate[0]), false},
		{"expired even when pinned", TLSAuto, &expired, hashBytes(expired.Certificate[0]), false},
		{"legacy remains pinned", "", &second, hashBytes(first.Certificate[0]), false},
		{"explicit pin remains strict", TLSPinned, &second, hashBytes(first.Certificate[0]), false},
		{"system trust valid CA", TLSSystem, &first, strings.Repeat("0", 64), true},
		{"system trust rejects untrusted pin", TLSSystem, &untrusted, hashBytes(untrusted.Certificate[0]), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			active.Store(tc.cert)
			client, err := clientForHub(srv.URL, tc.pin, tc.mode)
			if err != nil {
				t.Fatal(err)
			}
			client.Transport.(*http.Transport).TLSClientConfig.RootCAs = ca.pool
			defer client.CloseIdleConnections()
			before := requests.Load()
			r, err := client.Get(srv.URL)
			if r != nil {
				r.Body.Close()
			}
			if (err == nil) != tc.accepted {
				t.Fatalf("accepted=%v, want %v, err=%v", err == nil, tc.accepted, err)
			}
			if !tc.accepted && requests.Load() != before {
				t.Fatal("untrusted TLS connection reached HTTP and could disclose credentials")
			}
		})
	}
}

func TestHubClientRejectsAPIRedirect(t *testing.T) {
	var received atomic.Bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Store(true) }))
	defer target.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer srv.Close()
	client, err := clientForHub(srv.URL, hashBytes(srv.Certificate().Raw), TLSAuto)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	if r, err := client.Post(srv.URL, "application/json", strings.NewReader(`{"token":"secret"}`)); err == nil {
		r.Body.Close()
		t.Fatal("API redirect accepted")
	} else if r != nil {
		r.Body.Close()
	}
	if received.Load() {
		t.Fatal("pairing token was sent to a redirect target")
	}
}
