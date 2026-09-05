# swarmexec landing page

A tiny static site for the project, served at **https://swarm-exec.cloud-surfers.net**.

It shows the project description and four links:

- **GitLab repository** — source, docs & issues.
- **Docker Hub** — the agent image (`logleio/swarmexec-agent`).
- **Latest release** — a GitLab *permalink* that always resolves to the newest release.
- **Download CLI** — direct download of the latest release binary (per platform).

## Always current

The release/download links are GitLab **`/-/releases/permalink/latest`** URLs, so
they always point at the newest release **without rebuilding the page**. The build
also bakes the current version string into the page; on each release the CI
rebuilds `site:latest` and **Watchtower** refreshes the running service, so the
displayed version tracks releases too.

> The direct download permalinks rely on `filepath` being set on the release
> asset links (configured in `.gitlab-ci.yml`, `release-publish` job).

## Build

```sh
# from the repo root
docker build -f site/Dockerfile --build-arg VERSION=v1.2.3 -t swarmexec-site .
docker run --rm -p 8080:80 swarmexec-site   # open http://localhost:8080
```

CI builds and pushes `registry.logle.io/cs-public/swarm-remote-exec/site`.
`:latest` is refreshed both on a release (`:<version>` too, version baked in) and
when `site/` changes on the default branch (`:main` too). The deployed service
tracks `:latest`, and Watchtower redeploys it on each push.

## Deploy (Docker Swarm)

Routing/TLS is handled by the shared Traefik on the external `gateway` network
(same as the rest of the logle.io infra). On a manager:

```sh
docker stack deploy -c site/deploy/site-stack.yml swarmexec-site
```

Point a DNS record for `swarm-exec.cloud-surfers.net` at the swarm ingress; Traefik
requests the certificate via the `letsencrypt` resolver on first request.

## Hardening

The container runs **non-root**: it's built on `nginxinc/nginx-unprivileged`
(uid 101), so even the master process is unprivileged, and nginx serves on
**8080** (Traefik forwards to it). The stack additionally runs it with a
**read-only root filesystem** (a small `tmpfs` on `/tmp` holds nginx's pid and
temp files) and **drops all Linux capabilities** — a static file server needs
none. Verified: read-only rootfs blocks writes, and it still serves as uid 101.

## Files

- `index.html` — the page (self-contained, inline CSS, `__VERSION__` placeholder).
- `nginx.conf` — minimal nginx config (listens on 8080, gzip, security headers, `/healthz`).
- `Dockerfile` — `nginx-unprivileged` (non-root), bakes `VERSION` into the page.
- `deploy/site-stack.yml` — Swarm stack (Traefik labels, gateway net, non-root + read-only + cap-drop).
