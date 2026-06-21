package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidate_SelfSigned_WithSecret(t *testing.T) {
	c := &Config{SelfSigned: true, AgentSecret: "shh"}
	if err := c.Validate(); err != nil {
		t.Fatalf("self-signed + secret should be valid: %v", err)
	}
}

func TestValidate_SelfSigned_WithClientCA(t *testing.T) {
	c := &Config{SelfSigned: true, CACert: "/path/ca.crt"}
	if err := c.Validate(); err != nil {
		t.Fatalf("self-signed + client CA should be valid: %v", err)
	}
}

func TestValidate_SelfSigned_NoAuth(t *testing.T) {
	c := &Config{SelfSigned: true}
	if err := c.Validate(); err == nil {
		t.Fatal("self-signed without CA and without secret must fail")
	}
}

func TestValidate_NonSelfSigned_RequiresServerCert(t *testing.T) {
	c := &Config{CACert: "ca"} // missing server-cert/key
	if err := c.Validate(); err == nil {
		t.Fatal("non-self-signed must require server cert/key")
	}
}

func TestAgentSecretValue_File(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(p, []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Config{AgentSecretFile: p}
	v, err := c.AgentSecretValue()
	if err != nil {
		t.Fatal(err)
	}
	if v != "tok" {
		t.Fatalf("want trimmed 'tok', got %q", v)
	}
}
