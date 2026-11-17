// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package agenttest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte
}

func newCA(t testing.TB, name string) *ca {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &ca{cert: cert, key: key, der: der}
}

func serial(t testing.TB) *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// pki is the agent's certificate authority.
type pki struct{ ca *ca }

func newPKI(t testing.TB) *pki { return &pki{ca: newCA(t, "agenttest CA")} }

func writePEM(t testing.TB, path, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeCertPEM(t testing.TB, dir string, der []byte) string {
	t.Helper()
	p := filepath.Join(dir, "server-self-signed.pem")
	writePEM(t, p, "CERTIFICATE", der)
	return p
}

// issue signs a leaf for cn. A server leaf carries the loopback names; a client
// leaf carries only the CN (which may be empty, to test that refusal).
func (p *pki) issue(t testing.TB, signer *ca, cn string, server bool) (certDER []byte, keyDER []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.DNSNames = []string{"localhost"}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer.cert, &key.PublicKey, signer.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err = x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return der, keyDER
}

// writeServer writes the CA and a server certificate and key, returning the
// three paths in the order tlsconf.ServerConfig takes them.
func (p *pki) writeServer(t testing.TB, dir string) (caPath, certPath, keyPath string) {
	t.Helper()
	caPath = filepath.Join(dir, "ca.pem")
	certPath = filepath.Join(dir, "server.pem")
	keyPath = filepath.Join(dir, "server.key")
	writePEM(t, caPath, "CERTIFICATE", p.ca.der)
	der, keyDER := p.issue(t, p.ca, "agent", true)
	writePEM(t, certPath, "CERTIFICATE", der)
	writePEM(t, keyPath, "EC PRIVATE KEY", keyDER)
	return caPath, certPath, keyPath
}

var clientSeq atomic.Int64

func (p *pki) writeClient(t testing.TB, dir string, signer *ca, cn string) ClientCert {
	t.Helper()
	der, keyDER := p.issue(t, signer, cn, false)
	n := strconv.FormatInt(clientSeq.Add(1), 10)
	cert := filepath.Join(dir, "client-"+n+".pem")
	key := filepath.Join(dir, "client-"+n+".key")
	writePEM(t, cert, "CERTIFICATE", der)
	writePEM(t, key, "EC PRIVATE KEY", keyDER)
	return ClientCert{Cert: cert, Key: key}
}
