// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package stackfile

import (
	"os"
	"path/filepath"
	"testing"
)

// A stack file is read before anything is deployed — a diff, a plan, a deploy
// — and it may come from anywhere. Whatever its bytes, loading it must be a
// value or an error: the YAML layer, interpolation and the compose schema all
// sit on that path.
func FuzzLoadComposeFile(f *testing.F) {
	for _, s := range []string{
		"services:\n  web:\n    image: nginx:1.27\n    ports: ['8080:80']\n    deploy:\n      labels: {a: b}\n",
		"version: '3.8'\nservices:\n  web:\n    image: ${IMAGE:-nginx}\n    environment:\n      - A=${UNSET?required}\n",
		"x-common: &c\n  image: a\nservices:\n  a: *c\n  b:\n    <<: *c\n",
		"services:\n  web:\n    build: .\n    depends_on: [db]\n",
		"services: [",
		"",
		"\t\x00",
		"services:\n  web:\n    image: a\n    deploy:\n      replicas: 99999999999999999999\n",
	} {
		f.Add([]byte(s))
	}
	dir := f.TempDir()
	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(dir, "stack.yml")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = loadComposeFile(path)
		_, _ = ServiceLabels(path)
		_, _ = UnsupportedProperties(path)
	})
}

// A service with no body is a null, which the compose loader's own helper
// type-asserts into a panic. It must be an error — found by the fuzzer above.
func TestUnsupportedPropertiesBodylessService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stack.yml")
	if err := os.WriteFile(path, []byte("services:\n  web:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := UnsupportedProperties(path); err == nil {
		t.Error("a service without a body should be an error, not a panic")
	}
	if err := os.WriteFile(path, []byte("services:\n  web:\n    image: a\n    build: .\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := UnsupportedProperties(path); err != nil || len(got) != 1 || got[0] != "build" {
		t.Errorf("UnsupportedProperties = %v, %v; want [build]", got, err)
	}
}
