# Developing and releasing swarmexec

<!-- Moved out of the top-level README, unchanged; the README now keeps the essentials and points to https://swarm-exec.cloud-surfers.net for the rest. -->

Building from source, the documentation translations and the release pipeline. For using swarmexec, see the [documentation](https://swarm-exec.cloud-surfers.net/docs).

## Prerequisites

- **Go 1.27.2** to build — exactly the version `go.mod`, the CI image and the agent
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
```

Regenerating the gRPC code (only needed after editing the contract):

```sh
make tools           # installs buf + protoc-gen-go(-grpc) into $GOPATH/bin
make generate        # buf generate -> internal/pb/
make check-generate  # fail if internal/pb does not match the .proto (CI runs this)
```

### Documentation translations

`site/docs.html` (English) is the source of the docs page; `docs.de/es/fr/pl.html`
are derived from it, section by section. `site/i18n/` records which English
each translated section came from, and a test fails — naming the sections — as
soon as the English changes without the translations following.

```sh
make docs-i18n-check   # which sections are stale (no network)
make docs-i18n         # re-translate exactly those, via the Claude API (ANTHROPIC_API_KEY)
make docs-i18n-accept  # or translate by hand, then record them as up to date
```

`docs-i18n` sends each stale section with the English it was translated from
and the current translation, so only what changed is rewritten; every answer is
checked (same tables, command blocks, flags and keys; the head's language,
canonical and hreflang tags untouched) before it is written. CI checks on
every pipeline; it calls the API only when you start the optional, manual
`docs-i18n` job on a branch or merge request that changes the docs. That job
needs `ANTHROPIC_API_KEY` as a CI/CD variable and leaves the translations as a
`docs-i18n.patch` artifact — or, with `DOCS_I18N_PUSH_TOKEN` (a project access
token with `write_repository`) set, commits them to that branch.

### Releases (CI/CD)

[`.gitlab-ci.yml`](.gitlab-ci.yml) builds and versions automatically:

- **Branches / merge requests** — run tests and produce dev cli binaries
  (stamped with the short commit SHA) as job artifacts.
- **Default branch (`main`)** — additionally push a dev agent image
  (`$CI_REGISTRY_IMAGE/agent:main` and `:<short-sha>`).
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
    tag, `install.sh` from the live site, and that every site replica serves the
    new release. It turns the
    pipeline red if not (`ci/verify-release.sh`; run it by hand with a tag to
    check any release).

  A semver tag that is **not** on `main` fails the gate and builds nothing.

Cut a release:

```sh
git checkout main && git pull
git tag -a v1.2.3 -m "swarmexec v1.2.3"
git push origin v1.2.3
```

---
