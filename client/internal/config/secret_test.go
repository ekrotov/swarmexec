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
