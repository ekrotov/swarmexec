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
make cover           # race tests + coverage; fails below COVER_MIN (CI job `test`)
make lint            # staticcheck at the pinned STATICCHECK_VERSION (CI job `lint`)
make ci-tools        # every pinned tool: buf + plugins, staticcheck, govulncheck, scip-go
make release         # cli for every release platform -> ./dist + SHA256SUMS (CI job `build-cli`)
```

`PLATFORMS` in the Makefile is the single list of release platforms. Tool
versions live in [`tools.mk`](tools.mk); each tool is installed into
`$GOPATH/bin` once per version and Go toolchain (a stamp file remembers it), so
repeated `make tools` / `make lint` runs do not rebuild it. CI's
coverage minimum (`COVER_MIN`, generated `internal/pb` excluded) sits just
below the measured total: raise it when coverage grows, never lower it.
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

### Documentation site

`site/` is the website (landing page, docs, release notes) and is English
only; the privacy page also exists in German (`datenschutz.html`) and English (`datenschutz.en.html`). See
[`site/README.md`](site/README.md).

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
