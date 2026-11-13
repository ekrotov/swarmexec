// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package authmeta

import "testing"

// Bind's output is a wire format: every released client computes it and every
// released agent checks it. Fixed vectors freeze the scheme — the context
// string, its order before the certificate hash, and the unpadded base64url
// encoding — so a change to any of them fails here instead of locking out
// every older client in the field. The expected values were computed outside
// Go (Python hmac/hashlib/base64) from the formula in CONTRACT.md §2.1.
func TestBindVectors(t *testing.T) {
	der := make([]byte, 32)
	for i := range der {
		der[i] = byte(i)
	}
	cases := []struct {
		name   string
		secret string
		der    []byte
		want   string
	}{
		{"fixed secret and cert", "s3cret", der, "KjQSFXoQnqumMBOoRDfTJ32QkxlmolkW5sA3A8EBE3s"},
		{"empty cert", "s3cret", nil, "QjTaBazEBu2gkftjnyhECEmCP5oiDetY7AhiH2n6uvk"},
		{"other secret", "other", der, "2tcM34E6L3Xf8sTUGoulbGPLXJoJXAePmpHBRtt7eHY"},
	}
	for _, tc := range cases {
		if got := Bind(tc.secret, tc.der); got != tc.want {
			t.Errorf("%s: Bind = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The binding's point: the same secret over a different certificate is a
// different proof, so one captured on an attacker's connection is useless.
func TestBindDependsOnTheCertificate(t *testing.T) {
	if Bind("s3cret", []byte("cert-a")) == Bind("s3cret", []byte("cert-b")) {
		t.Fatal("proofs over different certificates must differ")
	}
}
