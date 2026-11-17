// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"

	"swarmexec/client/internal/swarmfake"
	"swarmexec/client/internal/swarmops"
)

// rotateSwarm: secret db (labelled for stack shop, content "old") mounted by
// service shop_web as /run/secrets/db_password, mode 0440; secret api_token
// that nothing uses; no tasks, so each service switch converges at once.
func rotateSwarm() *swarmfake.Swarm {
	web := stackedService("s-web", "shop_web", "shop", 1)
	web.ServiceStatus = nil
	web.Spec.TaskTemplate.ContainerSpec.Secrets = []*swarm.SecretReference{{
		SecretID: "sec-db", SecretName: "db",
		File: &swarm.SecretReferenceFileTarget{Name: "db_password", UID: "1000", GID: "1000", Mode: 0o440},
	}}
	secret := func(id, name, data string, labels map[string]string) swarm.Secret {
		return swarm.Secret{ID: id, Spec: swarm.SecretSpec{Annotations: swarm.Annotations{Name: name, Labels: labels}, Data: []byte(data)}}
	}
	return &swarmfake.Swarm{
		Services: []swarm.Service{web},
		Secrets: []swarm.Secret{
			secret("sec-db", "db", "old", map[string]string{swarmops.StackLabelKey: "shop", "team": "core"}),
			secret("sec-api", "api_token", "old-token", nil),
		},
	}
}

func secretByName(f *swarmfake.Swarm, name string) *swarm.Secret {
	for i := range f.Secrets {
		if f.Secrets[i].Spec.Name == name {
			return &f.Secrets[i]
		}
	}
	return nil
}

// webRef is the secret reference shop_web ends up with.
func webRef(t *testing.T, f *swarmfake.Swarm) *swarm.SecretReference {
	t.Helper()
	refs := f.Services[0].Spec.TaskTemplate.ContainerSpec.Secrets
	if len(refs) != 1 {
		t.Fatalf("shop_web has %d secret references", len(refs))
	}
	return refs[0]
}

// `secret rotate` end to end against the manager: the six steps leave the
// secret with its name, labels and new bytes, the service on it with its
// mount unchanged, and nothing of the temporary secret behind.
func TestSecretRotateRun(t *testing.T) {
	const value = "new value\n  with spaces\n" // byte for byte: nothing trimmed
	run := func(f *swarmfake.Swarm, stdin string, args ...string) (string, string, error) {
		t.Helper()
		g := cliCluster(t, f, nil)
		return runIO(t, newSecretCmd(g), stdin, append([]string{"rotate"}, args...)...)
	}
	rotated := func(t *testing.T, f *swarmfake.Swarm) {
		t.Helper()
		db := secretByName(f, "db")
		if db == nil || string(db.Spec.Data) != value {
			t.Fatalf("db = %+v, want the new bytes %q", db, value)
		}
		if db.Spec.Labels[swarmops.StackLabelKey] != "shop" || db.Spec.Labels["team"] != "core" || len(db.Spec.Labels) != 2 {
			t.Errorf("db labels = %v, want the original two and no marker", db.Spec.Labels)
		}
		if secretByName(f, "db_rotating") != nil {
			t.Error("the temporary secret was left behind")
		}
		ref := webRef(t, f)
		if ref.SecretName != "db" || ref.SecretID != db.ID {
			t.Errorf("shop_web uses %s (%s), want db (%s)", ref.SecretName, ref.SecretID, db.ID)
		}
		if ref.File == nil || ref.File.Name != "db_password" || ref.File.UID != "1000" || ref.File.GID != "1000" || ref.File.Mode != 0o440 {
			t.Errorf("mount = %+v: path, owner and mode must not change", ref.File)
		}
		if got := secretByName(f, "api_token"); got == nil || string(got.Spec.Data) != "old-token" {
			t.Errorf("an unrelated secret changed: %+v", got)
		}
	}

	t.Run("--from-file --yes", func(t *testing.T) {
		f := rotateSwarm()
		path := filepath.Join(t.TempDir(), "db.txt")
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		out, errOut, err := run(f, "", "db", "--from-file", path, "--yes", "--timeout", "5s")
		if err != nil {
			t.Fatal(err)
		}
		rotated(t, f)
		if out != "" {
			t.Errorf("stdout = %q", out)
		}
		for _, want := range []string{"[1/6]", "[2/6]", "[3/6]", "[4/6]", "[5/6]", "[6/6]", "rotated secret db\n"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("progress lacks %q:\n%s", want, errOut)
			}
		}
		if strings.Contains(errOut, "new value") || strings.Contains(errOut, "with spaces") {
			t.Errorf("a secret's content was printed:\n%s", errOut)
		}
	})

	t.Run("--from-stdin --yes", func(t *testing.T) {
		f := rotateSwarm()
		if _, _, err := run(f, value, "db", "--from-stdin", "--yes", "--timeout", "5s"); err != nil {
			t.Fatal(err)
		}
		rotated(t, f)
	})

	t.Run("an unused secret is replaced with nothing to restart", func(t *testing.T) {
		f := rotateSwarm()
		if _, _, err := run(f, "tok", "api_token", "--from-stdin", "--yes", "--timeout", "5s"); err != nil {
			t.Fatal(err)
		}
		if got := secretByName(f, "api_token"); got == nil || string(got.Spec.Data) != "tok" {
			t.Errorf("api_token = %+v", got)
		}
		if ref := webRef(t, f); ref.SecretID != "sec-db" {
			t.Errorf("shop_web was touched: %+v", ref)
		}
	})

	t.Run("a secret that does not exist is a failure", func(t *testing.T) {
		f := rotateSwarm()
		_, _, err := run(f, "x", "nope", "--from-stdin", "--yes")
		if exitCode(err) != 1 || !strings.Contains(err.Error(), "nope") {
			t.Errorf("err = %v", err)
		}
		if len(f.Secrets) != 2 {
			t.Errorf("secrets = %d, want the two it started with", len(f.Secrets))
		}
	})

	t.Run("the arguments are checked before the cluster is touched", func(t *testing.T) {
		empty := filepath.Join(t.TempDir(), "empty")
		if err := os.WriteFile(empty, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			name, stdin string
			args        []string
			want        string
		}{
			{"padded name", "x", []string{"db ", "--from-stdin", "--yes"}, "surrounding spaces"},
			{"no source", "", []string{"db", "--yes"}, "the new value is required"},
			{"two sources", "x", []string{"db", "--from-stdin", "--from-file", empty, "--yes"}, "not both"},
			{"empty file", "", []string{"db", "--from-file", empty, "--yes"}, "empty"},
			{"missing file", "", []string{"db", "--from-file", filepath.Join(t.TempDir(), "gone"), "--yes"}, "no such file"},
			{"directory", "", []string{"db", "--from-file", t.TempDir(), "--yes"}, "not a regular file"},
			{"empty stdin", "", []string{"db", "--from-stdin", "--yes"}, "empty"},
			{"timeout", "x", []string{"db", "--from-stdin", "--yes", "--timeout", "0s"}, "--timeout must be positive"},
		} {
			f := rotateSwarm()
			_, _, err := run(f, c.stdin, c.args...)
			if exitCode(err) != usageExitCode || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: err = %v, want usage error containing %q", c.name, err, c.want)
			}
			if string(secretByName(f, "db").Spec.Data) != "old" {
				t.Errorf("%s: db changed", c.name)
			}
		}
	})

	t.Run("without --yes the plan is shown and the operator asked", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "v")
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}

		f := rotateSwarm()
		asked := usePrompts(t, "n\n", true)
		_, errOut, err := run(f, "", "db", "--from-file", path)
		if exitCode(err) != 1 || !strings.Contains(err.Error(), "aborted") {
			t.Errorf("err = %v", err)
		}
		for _, want := range []string{"Services using db (1):", "shop_web → /run/secrets/db_password", "restarts twice", `belongs to stack "shop"`} {
			if !strings.Contains(errOut, want) {
				t.Errorf("the plan lacks %q:\n%s", want, errOut)
			}
		}
		if asked.String() != "Rotate secret db? [y/N]: " {
			t.Errorf("asked %q", asked.String())
		}
		if string(secretByName(f, "db").Spec.Data) != "old" || webRef(t, f).SecretID != "sec-db" {
			t.Error("the cluster changed after \"n\"")
		}

		usePrompts(t, "y\n", true)
		if _, _, err := run(f, "", "db", "--from-file", path, "--timeout", "5s"); err != nil {
			t.Fatal(err)
		}
		rotated(t, f)
	})

	t.Run("without a terminal --yes is required, and stdin is never the answer", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "v")
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		f := rotateSwarm()
		q := usePrompts(t, "y\n", false)
		_, _, err := run(f, "", "db", "--from-file", path)
		if exitCode(err) != usageExitCode || !strings.Contains(err.Error(), "pass --yes") || q.String() != "" {
			t.Errorf("no terminal: err %v asked %q", err, q.String())
		}
		// --from-stdin on a terminal: stdin is the value, so it cannot also
		// carry the answer.
		usePrompts(t, "y\n", true)
		_, _, err = run(f, value, "db", "--from-stdin")
		if exitCode(err) != usageExitCode || !strings.Contains(err.Error(), "pass --yes") {
			t.Errorf("--from-stdin: err %v", err)
		}
		if string(secretByName(f, "db").Spec.Data) != "old" {
			t.Error("db changed")
		}
	})

	t.Run("a rotation that stops half-way is finished by running it again", func(t *testing.T) {
		f := rotateSwarm()
		f.Fail = map[string]bool{"DELETE /secrets/sec-db": true} // step 3: remove the old secret
		_, _, err := run(f, value, "db", "--from-stdin", "--yes", "--timeout", "5s")
		if exitCode(err) != 1 || !strings.Contains(err.Error(), "re-run `swarmexec secret rotate db`") {
			t.Fatalf("err = %v, want the resume hint", err)
		}
		tmp := secretByName(f, "db_rotating")
		if tmp == nil || string(tmp.Spec.Data) != value || tmp.Spec.Labels[swarmops.RotateMarkerLabel] != "db" {
			t.Fatalf("the stand-in = %+v, want it to hold the new content and its marker", tmp)
		}
		if ref := webRef(t, f); ref.SecretName != "db_rotating" || ref.File.Name != "db_password" {
			t.Errorf("shop_web should be parked on the stand-in with its mount: %+v", ref)
		}

		// The run that finishes it says so when asked.
		f.Fail = nil
		asked := usePrompts(t, "y\n", true)
		_, errOut, err := run(f, "", "db", "--from-file", writeTemp(t, value), "--timeout", "5s")
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if !strings.Contains(errOut, "Resuming an interrupted rotation of db") || asked.String() == "" {
			t.Errorf("resume prompt:\n%s", errOut)
		}
		rotated(t, f)
	})
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "v")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// readRotateValue refuses more than swarm accepts, without reading the rest.
func TestReadRotateValueLimit(t *testing.T) {
	big := strings.NewReader(strings.Repeat("a", swarmops.MaxSecretSize+10))
	data, err := readRotateValue(&secretRotateFlags{fromStdin: true}, big)
	if err != nil || len(data) != swarmops.MaxSecretSize+1 {
		t.Fatalf("read %d bytes, %v", len(data), err)
	}
	if err := swarmops.ValidateRotateValue(data); err == nil || !strings.Contains(err.Error(), "500 KB") {
		t.Errorf("an oversized value passed validation: %v", err)
	}
}
