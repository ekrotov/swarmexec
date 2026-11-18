# Developing and releasing swarmexec

<!-- Moved out of the top-level README, unchanged; the README now keeps the essentials and points to https://swarm-exec.cloud-surfers.net for the rest. -->

Building from source, the documentation site and the release pipeline. For using swarmexec, see the [documentation](https://swarm-exec.cloud-surfers.net/docs).

## Prerequisites

- **Go 1.27.4** to build — exactly the version `go.mod`, the CI image and the agent
  Dockerfile pin (they move together; a Go security patch bumps all three).
- **Docker Swarm** already initialised, and access to a **manager** node.
- TLS certificates (see step 2.1). The project does **no** PKI bootstrap — certs
  come from your own CA. A dev-cert script is included for testing.

---

## Build

All `make` targets run from the **repository root**.

```sh
make build-all       # both binaries -> ./bin/swarmexec (cli) and ./bin/agent
make build           # cli only      -> ./bin/swarmexec
make agent           # agent only    -> ./bin/agent
make agent-image     # agent container image (swarmexec-agent:<version>)
make test            # unit tests for the whole module
make cover           # race + shuffled tests + coverage; fails below COVER_MIN or a per-file floor (CI job `test`)
make fuzz            # every fuzz target for FUZZTIME (default 15s) each; not in CI — `make cover` replays their seeds and saved corpus
make mod-check       # go.mod/go.sum must be tidy: `go mod tidy -diff` (CI job `test`)
make lint            # staticcheck at the pinned STATICCHECK_VERSION + `make cyclo` (no function over complexity 15) (CI job `lint`)
make ci-tools        # every pinned tool: buf + plugins, staticcheck, gocyclo, govulncheck, scip-go
make release         # cli for every release platform -> ./dist + SHA256SUMS (CI job `build-cli`)
```

`PLATFORMS` in the Makefile is the single list of release platforms. Tool
versions live in [`tools.mk`](tools.mk); each tool is installed into
`$GOPATH/bin` once per version and Go toolchain (a stamp file remembers it), so
repeated `make tools` / `make lint` runs do not rebuild it. CI's
coverage minimum (`COVER_MIN`, generated `internal/pb` excluded) sits just
below the measured total: raise it when coverage grows, never lower it.
The total averages risk away, so `make cover` also holds the files listed in
[`ci/cover-floors.txt`](ci/cover-floors.txt) (exec, port-forward, the destructive
commands, secret rotation, stack deploy and the agent handlers behind them) to a
per-file floor, checked by `ci/cover-floors.sh`; `ci/cover-floors.sh --report`
prints every file's figure.
GitLab reads the total from the `test` job log (`coverage:` regex), shows
it as the coverage badge on the project page and charts it over time under
*Analyze > Repository analytics*; both are visible to project members only,
like the pipelines.

Regenerating the gRPC code (only needed after editing the contract):

```sh
make tools           # installs buf + protoc-gen-go(-grpc) into $GOPATH/bin
make generate        # buf generate -> internal/pb/
make check-generate  # fail if internal/pb does not match the .proto (CI runs this)
make proto-check     # buf lint + buf breaking against BUF_AGAINST (default origin/main)
```

CI (`generate-check`) runs `proto-check` against the MR's target branch, and
on branches and tags against the newest release tag. `buf breaking` uses the
strict `FILE` rules: a removed or renumbered field fails the pipeline, as
CONTRACT.md §7 asks. The lint exceptions in `buf.yaml` are wire names that
cannot change without breaking deployed agents.

### Test hygiene

- **Fuzz targets** (`FuzzXxx`, next to the parser they exercise) cover what
  reads untrusted input: log formats, forward frames, policy files, prune
  schedule labels, the env editor text, port specs, service-edit strings, stack
  files and target resolution. They are seeded, so plain `go test` replays them
  in milliseconds; `make fuzz` explores for `FUZZTIME` each. An input that breaks
  one lands in the package's `testdata/fuzz/`: fix the code and commit the file.
- **Goroutine leaks:** packages that start goroutines call
  `leakcheck.Main(m)` from `TestMain` (`internal/leakcheck`), which fails the run
  if one outlives the tests. Add it to a new package of that kind.
- **Global state** a test replaces (the client logger, a stubbed function) is put
  back with `t.Cleanup` (`clientlog.Save()` for the logger), and CI runs the
  suites with `-shuffle=on`, so an order dependence fails there.

### Testing the client against the real agent (`agenttest`)

`client/` and `agent/` do not import each other, and Go's `internal` rule keeps
a test in one from reaching the other, so a hand-written fake agent in a client
test only ever proves the author's idea of the agent. `agent/agenttest` is the
one sanctioned door: it starts the **real** agent (`server.New`, the real
interceptor chain `server.Interceptors`, `tlsconf`, the policy loader, every RPC
handler and `dockerStatus`) on a 127.0.0.1 listener with a fake Docker daemon
behind it. Client tests import it:

```go
d := agenttest.NewDocker().
    AddContainer(agenttest.Container{ID: "c1", Service: "web"}).
    SetLogs("c1", "hello\nERR oops\n")           // "ERR " lines go to stderr
a := agenttest.Start(t, agenttest.Options{
    Secret: "s3cret",   // shared-secret mode (self-signed server certificate)
    // MTLS: true,      // or: require client certificates from a test CA
    // Policy: "...",   // contents of a -policy-file
    Docker: d,
})
ac := agentcall.New(config.Config{Port: 7000, Insecure: true,
    AgentSecret: "s3cret", ProxyDialer: a.Dial}) // a.Dial ignores the node name
```

What the handle gives you: `Host`/`Port`/`Addr()`, `CAFile` (verifies the
server certificate; use `ServerName: "localhost"`), `Dial` (a `ProxyDialer`),
`IssueClient(t, cn)` / `IssueForeignClient(t, cn)` for mTLS client
certificates (`ClientCert{Cert, Key}`), `Audit()` / `AuditEvents(name)` for the
decoded audit log, and `Docker`, the fake daemon: `Fail("ContainerList", err)`
injects an Engine error (wrap a `cerrdefs` class to see the status mapping),
`Calls()` / `Called()` / `LogCalls()` record what the agent asked of Docker,
`SetExec(fn)` scripts the exec process. Unimplemented Engine methods (images,
prune, stats, sidecars) panic, which the recovery interceptor reports as
`Internal`; add them to `agenttest/docker.go` when a test needs one. The
background caches (volume sizes, stats, reclaim, prune schedule) are not
started.

Use `agenttest` whenever a test's value is in what the *agent* does with the
request (credentials, identity, policy, status codes, audit, translation into
Docker calls). Keep a hand-written fake only for what a real agent cannot
produce: a node that predates an RPC, a dead link, scripted slowness, counting
TLS handshakes or dials (`agentcall/fake_test.go`). `agenttest` mirrors
`buildServer`'s wiring in `cmd/agent/main.go`; change the two together.

### Documentation site

`site/` is the website (landing page, docs, release notes) and is English
only; the privacy page also exists in German (`datenschutz.html`) and English (`datenschutz.en.html`). See
[`site/README.md`](site/README.md).

The CLI reference page (`site/docs/cli.html`, served as `/docs/cli`) is
generated from the command tree: after adding or changing a command or flag,
run `make cli-docs` and commit the page. CI's `test` job runs
`make cli-docs-check`, which regenerates it and fails on any difference.

### TUI keys

Before adding a key, a tab or an overlay to `swarmexec ui`, read
[`KEYMAP.md`](KEYMAP.md): the global keys, the detail verb, the `Ctrl-S`
commit idiom, red and uppercase danger keys, Esc-first footers, and the
tables in `keymap.go` whose tests keep tabs and overlays conflict-free.

### Releases (CI/CD)

[`.gitlab-ci.yml`](.gitlab-ci.yml) builds and versions automatically:

- **Merge requests** — run the tests and produce dev cli binaries for
  linux/amd64 and windows/amd64 (stamped with the short commit SHA) as job
  artifacts; those two cover the code's one platform split. A push to a
  branch **without** an MR runs no pipeline (open a draft MR, or use
  *Build > Pipelines > Run pipeline*): before, every MR push ran twice, once
  as a branch pipeline and once as an MR pipeline.
- **Default branch (`main`)** — all five cli platforms, and additionally push
  a dev agent image (`$CI_REGISTRY_IMAGE/agent:main` and `:<short-sha>`).
- **Semantic-version tag on `main`** — cut a release. A release is built
  **exactly when** the tag matches `vMAJOR.MINOR.PATCH` (e.g. `v1.2.3`,
  optionally `-rc.1` / `+build`) **and** the tagged commit is on `main`
  (enforced by the `verify-tag-on-main` gate). It produces:
  - cli binaries for linux/macOS/windows stamped with the version,
  - the agent image at `$CI_REGISTRY_IMAGE/agent:<version>` and `:latest`,
  - the **GitHub release** (`mirror-github`), which holds the binaries — every
    download points there: install.sh, Homebrew, Scoop, the site; a GitLab
    **Release** whose asset links point at those GitHub files (GitLab's package
    registry is not used: this instance loses package files whenever its
    container is recreated),
  - the Homebrew formula and Scoop manifest, the site,
  - and last, `verify-release`: it checks from the outside, anonymously, that
    all of it arrived — the GitHub downloads against the build's `SHA256SUMS`,
    the GitLab release links, the formula and manifest hashes, the Docker Hub
    tag and `install.sh` from the live site. It turns the pipeline red if not
    (`ci/verify-release.sh`; run it by hand with a tag to check any release).
    `verify-site` checks that every site replica serves the new release; it
    is non-blocking because the site rollout stalls now and then for a known
    infrastructure reason, and its log says what to run
    (`docker service update --force swarm-exec-site_site`), then retry it.

  A semver tag that is **not** on `main` fails the gate and builds nothing.

How the pipeline stays fast (details in the comments of `.gitlab-ci.yml`):

- **DAG, not stages.** Jobs start as soon as their `needs:` are met:
  `build-cli` runs alongside the tests. Every job that publishes something
  (images, release, mirror, packages, site deploy) lists the quality gate
  `.release-gate` explicitly: test, lint, generate-check and vulncheck (and
  `verify-tag-on-main` on a tag) must pass first.
- **Runner.** Every job carries the tag `no-docker` and runs on the project
  runner `swarmexec-no-docker`: unprivileged, without the host's
  `docker.sock`. No job needs either (Kaniko builds the images). The shared
  instance runner mounts `docker.sock` privileged, which makes any of its
  jobs root on the host.
- **Caches.** Go's build cache and module cache are not GitLab caches: each
  runner host mounts the Docker volumes `go-build-cache` and `go-mod-cache`
  at `/go-cache/build` and `/go-cache/mod` into every job (runner
  `config.toml`, `[runners.docker] volumes`), and CI sets `GOCACHE` /
  `GOMODCACHE` to them. One copy per host, shared by its slots, nothing
  zipped. Every Go job logs whether its host mounts them and warns if not
  (the job then runs cold). Tag (release) pipelines do not use the volumes,
  because any job on a host can write them: they build cold from
  project-local caches and run `go mod verify` first. Only the tool binaries (keyed on `tools.mk`) and
  staticcheck's analysis cache remain GitLab caches; without a cache server
  the runners keep those per host *and concurrent slot*, so every job that
  uses one also writes it.
- **Auto-cancel.** A newer push to the same MR or to `main` cancels the older
  pipeline's interruptible jobs: checks and builds. Registry pushes, releases,
  the mirror, packages and deploys are never interrupted, and a `main`
  pipeline that changes `site/` is never cancelled (its site image would be
  lost otherwise).

Cut a release:

```sh
git checkout main && git pull
git tag -a v1.2.3 -m "swarmexec v1.2.3"
git push origin v1.2.3
```

---
