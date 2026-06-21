package tlsconf

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"time"
)

// selfSignedValidity is how long the generated cert is valid. It is generous
// because the cert is regenerated on every agent restart anyway; this only
// avoids expiry during very long uptimes.
const selfSignedValidity = 10 * 365 * 24 * time.Hour

// SelfSignedServerConfig builds a TLS config whose server certificate is
// generated and self-signed at startup, carrying the given SANs. Clients are
// expected to skip server-cert verification (the cert chains to nothing) and
// authenticate via the shared secret instead.
//
// If clientCAPath is non-empty, client certificates are still required and
// verified against that CA (so per-operator identity is preserved); otherwise
// no client certificate is requested.
func SelfSignedServerConfig(sans []string, clientCAPath string, now time.Time) (*tls.Config, []string, error) {
	dnsNames, ips := parseSANs(sans)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("generate serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "swarmexec-agent"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate: %w", err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: tmpl}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   tls.NoClientCert,
	}
	if clientCAPath != "" {
		caPEM, err := os.ReadFile(clientCAPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read client CA cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, nil, fmt.Errorf("client CA cert file contained no valid certificates")
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}

	// Describe the SANs for logging.
	desc := make([]string, 0, len(dnsNames)+len(ips))
	for _, d := range dnsNames {
		desc = append(desc, "DNS:"+d)
	}
	for _, ip := range ips {
		desc = append(desc, "IP:"+ip.String())
	}
	return cfg, desc, nil
}

// parseSANs splits SAN specifiers into DNS names and IPs. Each entry may be
// "DNS:name", "IP:addr", or a bare value (classified as an IP if it parses as
// one, else a DNS name). Duplicates and empties are dropped.
func parseSANs(sans []string) (dnsNames []string, ips []net.IP) {
	seenDNS := map[string]bool{}
	seenIP := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		var kind, val string
		if i := strings.IndexByte(s, ':'); i >= 0 && (strings.EqualFold(s[:i], "DNS") || strings.EqualFold(s[:i], "IP")) {
			kind, val = strings.ToUpper(s[:i]), strings.TrimSpace(s[i+1:])
		} else {
			val = s
		}
		if ip := net.ParseIP(val); ip != nil && kind != "DNS" {
			if !seenIP[ip.String()] {
				seenIP[ip.String()] = true
				ips = append(ips, ip)
			}
			return
		}
		if !seenDNS[val] {
			seenDNS[val] = true
			dnsNames = append(dnsNames, val)
		}
	}
	for _, entry := range sans {
		for _, part := range strings.Split(entry, ",") {
			add(part)
		}
	}
	return dnsNames, ips
}
