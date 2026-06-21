package tlsconf

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSelfSignedServerConfig_SANsAndNoClientCert(t *testing.T) {
	cfg, desc, err := SelfSignedServerConfig(
		[]string{"DNS:swarmexec-agent", "DNS:docker1", "IP:127.0.0.1", "IP:10.0.0.5"},
		"", time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Errorf("without CA, ClientAuth should be NoClientCert, got %v", cfg.ClientAuth)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("want 1 certificate, got %d", len(cfg.Certificates))
	}
	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if !contains(leaf.DNSNames, "swarmexec-agent") || !contains(leaf.DNSNames, "docker1") {
		t.Errorf("DNS SANs missing: %v", leaf.DNSNames)
	}
	var haveLoop, haveNode bool
	for _, ip := range leaf.IPAddresses {
		if ip.String() == "127.0.0.1" {
			haveLoop = true
		}
		if ip.String() == "10.0.0.5" {
			haveNode = true
		}
	}
	if !haveLoop || !haveNode {
		t.Errorf("IP SANs missing: %v", leaf.IPAddresses)
	}
	if len(desc) == 0 {
		t.Error("expected SAN description for logging")
	}

	// The cert must actually validate for the node address (the original bug).
	if err := leaf.VerifyHostname("10.0.0.5"); err != nil {
		t.Errorf("cert should be valid for node IP: %v", err)
	}
}

func TestSelfSignedServerConfig_WithClientCA(t *testing.T) {
	caPath := writeTestCA(t)
	cfg, _, err := SelfSignedServerConfig([]string{"DNS:swarmexec-agent"}, caPath, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("with CA, client certs should be required, got %v", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Error("ClientCAs should be set when a CA is provided")
	}
}

func TestParseSANs(t *testing.T) {
	dns, ips := parseSANs([]string{"DNS:foo", "IP:1.2.3.4", "bar", "5.6.7.8", "DNS:foo"})
	if !contains(dns, "foo") || !contains(dns, "bar") {
		t.Errorf("dns names: %v", dns)
	}
	if len(dns) != 2 {
		t.Errorf("foo should be de-duplicated: %v", dns)
	}
	var got []string
	for _, ip := range ips {
		got = append(got, ip.String())
	}
	if !contains(got, "1.2.3.4") || !contains(got, "5.6.7.8") {
		t.Errorf("ips: %v", got)
	}
	// Comma-separated entries are split.
	dns2, ips2 := parseSANs([]string{"DNS:a,IP:9.9.9.9"})
	if !contains(dns2, "a") || len(ips2) != 1 {
		t.Errorf("comma split failed: dns=%v ips=%v", dns2, ips2)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// writeTestCA generates a throwaway CA cert and writes it to a temp file.
func writeTestCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.crt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	return path
}
