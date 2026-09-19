// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package dial

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"testing"

	"google.golang.org/grpc/credentials"

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

// An absent CA must NOT silently mean "accept any server". It used to, which
// made "I have not configured a CA" and "I accept an unverified agent" the same
// state — reachable without anything in the configuration saying so.
func TestLoadTLS_MissingCAIsAnErrorNotSilentSkipVerify(t *testing.T) {
	cfg := config.Default() // no CA, no insecure flag
	if _, err := loadTLS(cfg); err == nil {
		t.Fatal("a config with neither CA nor insecure must be refused")
	}
}

func TestBearer_SendsABoundProofNotTheSecret(t *testing.T) {
	certDER := []byte("pretend-certificate-der")
	ctx := requestInfoCtx(certDER)

	b := bearer{secret: "tok", operator: "alice"}
	md, err := b.GetRequestMetadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The whole point: the credential is not on the wire.
	if _, ok := md[authmeta.SecretKey]; ok {
		t.Error("the raw secret must not be sent")
	}
	if md[authmeta.BindingKey] != authmeta.Bind("tok", certDER) {
		t.Errorf("binding metadata: %v", md)
	}
	if md[authmeta.OperatorKey] != "alice" {
		t.Errorf("operator metadata: %v", md)
	}
	if !b.RequireTransportSecurity() {
		t.Error("bearer must require transport security")
	}

	// A different server certificate yields a different proof — that is what
	// makes a captured one useless anywhere else.
	other, _ := b.GetRequestMetadata(requestInfoCtx([]byte("another-cert")))
	if other[authmeta.BindingKey] == md[authmeta.BindingKey] {
		t.Error("the proof must differ per server certificate")
	}

	// Operator omitted when empty.
	md2, _ := bearer{secret: "tok"}.GetRequestMetadata(ctx)
	if _, ok := md2[authmeta.OperatorKey]; ok {
		t.Error("empty operator should be omitted")
	}
}

// Without TLS information there is nothing to bind to. Refusing is the point:
// sending the secret in the one case we cannot reason about is the old bug.
func TestBearer_RefusesWhenItCannotBind(t *testing.T) {
	if _, err := (bearer{secret: "tok"}).GetRequestMetadata(context.Background()); err == nil {
		t.Error("a call with no TLS info must not produce credentials")
	}
	// …unless the operator explicitly opted into the legacy form.
	md, err := (bearer{secret: "tok", legacy: true}).GetRequestMetadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if md[authmeta.SecretKey] != "tok" {
		t.Errorf("legacy mode should fall back to the raw secret: %v", md)
	}
}

// The legacy opt-in sends both, so a mixed fleet keeps working during a roll
// forward.
func TestBearer_LegacySendsBoth(t *testing.T) {
	certDER := []byte("cert")
	md, err := (bearer{secret: "tok", legacy: true}).GetRequestMetadata(requestInfoCtx(certDER))
	if err != nil {
		t.Fatal(err)
	}
	if md[authmeta.SecretKey] != "tok" || md[authmeta.BindingKey] != authmeta.Bind("tok", certDER) {
		t.Errorf("legacy mode should send both forms: %v", md)
	}
}

// requestInfoCtx fakes what gRPC puts in the per-RPC context for a TLS call.
func requestInfoCtx(certDER []byte) context.Context {
	cert := &x509.Certificate{Raw: certDER}
	return credentials.NewContextWithRequestInfo(context.Background(), credentials.RequestInfo{
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}},
		},
	})
}
