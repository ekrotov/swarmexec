# Contributing to swarmexec

Thanks for your interest in swarmexec. This document covers the legal side of
contributing; for how the code is laid out, start with the
[`README.md`](README.md) and [`CONTRACT.md`](CONTRACT.md).

## License of contributions

swarmexec is licensed under the [Apache License 2.0](LICENSE). By contributing,
you agree that your contribution is licensed under those same terms, as stated
in section 5 of the License:

> Unless You explicitly state otherwise, any Contribution intentionally
> submitted for inclusion in the Work by You to the Licensor shall be under the
> terms and conditions of this License, without any additional terms or
> conditions.

We do **not** require a separate Contributor License Agreement. We do require a
Developer Certificate of Origin sign-off on every commit.

## Developer Certificate of Origin (DCO)

Every commit must carry a `Signed-off-by` line. It certifies that you wrote the
patch, or otherwise have the right to submit it under the project's license —
the full text is the [Developer Certificate of Origin 1.1](https://developercertificate.org/).

Add the line automatically with:

```sh
git commit -s
```

which appends:

```
Signed-off-by: Your Name <your.email@example.com>
```

Use your real name and a reachable email address. The name and email must match
the commit author. If you forgot the sign-off, amend the last commit with
`git commit -s --amend`, or for a whole branch:
`git rebase --signoff main`.

## Copyright headers

New source files carry the same two-line header as the rest of the tree:

```go
// Copyright 2026 Cloud Surfers GmbH
// SPDX-License-Identifier: Apache-2.0
```

Contributors keep the copyright in their own contributions — the header names
the project's maintaining entity and does not transfer your rights. If you
prefer your own copyright line on files you author, add it below the existing
one rather than replacing it.

Generated code under `internal/pb/` is not edited by hand; it inherits its
header from `proto/swarmexec.proto`.

## Before you open a merge request

- `go build ./...` and `go test ./...` pass.
- `gofmt -l .` reports nothing.
- Protocol changes are reflected in [`CONTRACT.md`](CONTRACT.md) — it is the
  authoritative wire spec, and the agent and cli are versioned together.

## Maintainer notes

If the canonical repository ever moves, the source URL is referenced in exactly
these places and they must be updated together: [`NOTICE`](NOTICE) (the
attribution downstream users are required to carry), `dockerhub/overview.md`,
and `site/index.html`.

## Maintainer

Cloud Surfers GmbH — <eugen.krotov@cloud-surfers.de>
