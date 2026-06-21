package dial

import (
	"testing"

	"swarmexec/client/internal/config"
	"swarmexec/internal/authmeta"
)

func TestLoadTLS_InsecureSkipsVerify(t *testing.T) {
	cfg := config.Default()
	cfg.AgentSecret = "s"
	cfg.Insecure = true
	tc, err := loadTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !tc.InsecureSkipVerify {
		t.Error("insecure config should set InsecureSkipVerify")
	}
	if len(tc.Certificates) != 0 {
		t.Error("no client cert configured -> Certificates should be empty")
	}
}

func TestLoadTLS_NoCAImpliesSkipVerify(t *testing.T) {
	cfg := config.Default() // no CA, no insecure flag
	tc, err := loadTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !tc.InsecureSkipVerify {
		t.Error("missing CA should imply skip-verify (self-signed agent)")
	}
}

func TestBearer_Metadata(t *testing.T) {
	b := bearer{secret: "tok", operator: "alice"}
	md, err := b.GetRequestMetadata(nil)
	if err != nil {
		t.Fatal(err)
	}
	if md[authmeta.SecretKey] != "tok" {
		t.Errorf("secret metadata: %v", md)
	}
	if md[authmeta.OperatorKey] != "alice" {
		t.Errorf("operator metadata: %v", md)
	}
	if !b.RequireTransportSecurity() {
		t.Error("bearer must require transport security")
	}

	// Operator omitted when empty.
	md2, _ := bearer{secret: "tok"}.GetRequestMetadata(nil)
	if _, ok := md2[authmeta.OperatorKey]; ok {
		t.Error("empty operator should be omitted")
	}
}
