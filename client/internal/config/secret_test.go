// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func baseSecretCfg() Config {
	c := Default()
	c.AgentSecret = "shh"
	return c
}

func TestValidate_SecretMode_InsecureOK(t *testing.T) {
	c := baseSecretCfg()
	c.Insecure = true // no CA, skip verify
	if err := c.Validate(); err != nil {
		t.Fatalf("secret + insecure should be valid: %v", err)
	}
}

func TestValidate_SecretMode_NoCANoInsecure(t *testing.T) {
	c := baseSecretCfg() // no CA, not insecure
	if err := c.Validate(); err == nil {
		t.Fatal("secret mode without CA and without insecure should fail")
	}
}

func TestValidate_SecretMode_CertWithoutKey(t *testing.T) {
	c := baseSecretCfg()
	c.Insecure = true
	c.Cert = "/tmp/x.crt" // key missing
	if err := c.Validate(); err == nil {
		t.Fatal("cert without key should fail")
	}
}

func TestValidate_DefaultStillRequiresMTLS(t *testing.T) {
	c := Default() // no secret
	if err := c.Validate(); err == nil {
		t.Fatal("non-secret mode must still require ca/cert/key")
	}
}

func TestAgentSecretValue_File(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte("  filetoken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Default()
	c.AgentSecretFile = p
	got, err := c.AgentSecretValue()
	if err != nil {
		t.Fatal(err)
	}
	if got != "filetoken" {
		t.Fatalf("want trimmed file content, got %q", got)
	}
	if !c.SecretMode() {
		t.Fatal("SecretMode should be true when AgentSecretFile is set")
	}
}

// Secret mode still checks every TLS path it is given: a CA, and a cert with
// its key, must be readable; all present and readable is valid.
func TestValidate_SecretMode_PathsReadable(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ok")
	if err := os.WriteFile(ok, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")
	for _, tc := range []struct {
		name          string
		ca, cert, key string
		wantErr       bool
	}{
		{"ca unreadable", missing, "", "", true},
		{"ca readable", ok, "", "", false},
		{"cert unreadable", ok, missing, ok, true},
		{"key unreadable", ok, ok, missing, true},
		{"all readable", ok, ok, ok, false},
	} {
		c := baseSecretCfg()
		c.CA, c.Cert, c.Key = tc.ca, tc.cert, tc.key
		if err := c.Validate(); (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, want error %v", tc.name, err, tc.wantErr)
		}
	}
}

func TestValidate_BadPortAndUnreadableMTLS(t *testing.T) {
	c := Default()
	c.Port = 70000
	if err := c.Validate(); err == nil {
		t.Error("port 70000 should fail")
	}
	c = Default()
	c.CA, c.Cert, c.Key = "/nonexistent/ca", "/nonexistent/cert", "/nonexistent/key"
	if err := c.Validate(); err == nil {
		t.Error("unreadable mTLS paths should fail")
	}
}
