# swarmexec website

A small static site for the project, served at **https://swarm-exec.cloud-surfers.net**
by nginx (non-root, port 8080) behind Traefik.

| URL | File | What |
|---|---|---|
| `/` | `index.html` | landing page |
| `/docs` | `docs.html` | client documentation — every command, flag and key |
| `/examples` | `examples.html` | recipes: whole workflows, CLI and TUI side by side |
| `/docker-swarm-exec-alternatives` (`/compare` redirects) | `compare.html` | swarmexec against SSH, docker contexts, Portainer, Swarmpit, lazydocker, socat tunnels and kubectl, with an FAQ |
| `/releases` | `releases.html` | release notes, rendered from `releases.json` |
| `/datenschutz`, `/datenschutz.en.html` | `datenschutz.html`, `datenschutz.en.html` | privacy policy, German and English |
| `/install.sh` | `install.sh` | the installer (`curl -fsSL …/install.sh \| sh`) |
| `/healthz` | — | `200 ok`, for the container healthcheck |

The site is English only (the privacy page aside). The former translations
(`index.*`, `docs.*`, `releases.*` in de/es/fr/pl, `datenschutz.es/fr/pl`) are
gone; nginx answers their URLs — with or without `.html` — with a permanent
redirect to the English page.

## Always current

The download links point at the **GitHub** release:
`https://github.com/ekrotov/swarmexec/releases/latest/download/<file>`, so they
always resolve to the newest release **without rebuilding the page**. The binaries
are GitHub release assets, uploaded by the `mirror-github` job; GitLab's package
registry is not used, because this GitLab instance keeps package files only until
its container is next recreated. The other links on the page go to the GitHub
repository (source, releases, issues), the GitLab original it is mirrored from,
and the agent image on Docker Hub (`logleio/swarmexec-agent`).

The build bakes the version into `index.html`, `docs.html`, `examples.html` and `compare.html`
(the `__VERSION__` placeholder, also used as `?v=` on stylesheets and scripts),
stamps the build date into `sitemap.xml` (`<lastmod>`), and fetches the tag list
from the GitLab API into `releases.json` for the release-notes page.

## How a change gets live

From [`.gitlab-ci.yml`](../.gitlab-ci.yml):

- **Merge to `main` touching `site/`** — `site-image` builds the image with
  `VERSION=latest` and pushes `:latest` and `:main` to
  `registry.logle.io/cs-public/swarm-remote-exec/site`. Nothing in this repository
  deploys it.
- **Release tag** (`vX.Y.Z`) — `site-image` pushes `:vX.Y.Z`, `:X.Y.Z` and, for a
  stable release, `:latest`; then `deploy-site` triggers the pipeline of
  `infrastruktur/swar-exec-site`, which renders the stack with that version and
  deploys it. `ci/verify-release.sh` then checks that every replica serves the
  release and that its notes list it.

So a site-only change merged to `main` is built at once, but the running site is
the one the last release deployed — unless the deployment outside this repository
follows `:latest` (the comment on `site-image` says Watchtower does; the
`deploy-site` path pins the version). Check there before counting on a merge
going live by itself.

## Build and try locally

```sh
# from the repo root
docker build -f site/Dockerfile --build-arg VERSION=v1.2.3 -t swarmexec-site .
docker run --rm --read-only --tmpfs /tmp --user 101:101 --cap-drop ALL \
  -p 8080:8080 swarmexec-site          # open http://127.0.0.1:8080
```

The container listens on **8080**. The flags mirror the stack's hardening, so what
works here works there. Without network access to `gitlab.logle.io` the build
still succeeds, with an empty `releases.json`.

## Deploy (Docker Swarm)

Routing and TLS are handled by the shared Traefik on the external `gateway` network
(same as the rest of the logle.io infra). On a manager:

```sh
docker stack deploy -c site/deploy/site-stack.yml swarmexec-site
```

The stack uses `${SWARMEXEC_SITE_IMAGE}`, by default the registry's `:latest`.
Point a DNS record for `swarm-exec.cloud-surfers.net` at the swarm ingress; Traefik
requests the certificate via the `letsencrypt` resolver on first request.

## Hardening

The container runs **non-root**: it is built on `nginxinc/nginx-unprivileged`
(uid 101), so even the master process is unprivileged, and nginx serves on
**8080** (Traefik forwards to it). The stack additionally runs it with a
**read-only root filesystem** (a small `tmpfs` on `/tmp` holds nginx's pid and
temp files) and **drops all Linux capabilities** — a static file server needs
none. The healthcheck probes `127.0.0.1`, not `localhost`: nginx binds IPv4 only,
and busybox `wget` tries `::1` without falling back.

## Files

- `index.html` — the landing page.
- `docs.html` — the documentation. Command and TUI headings carry ids
  (`#exec`, `#logs`, `#port-forward`, `#tui-secrets`, …) for deep links.
- `examples.html` — the recipes. Every flag and key in it is checked against
  `client/internal/cli` (and `keymap.go` for the TUI); keep it that way when a
  command changes.
- `compare.html` — the comparison with other ways to exec, forward and read logs on a
  Swarm. Its headings and FAQ are phrased like the searches it answers; the FAQ's
  JSON-LD (`FAQPage`) must say the same as the visible FAQ. swarmexec claims are
  checked against `client/internal/cli`; third-party ones carry a checked-on date.
- `releases.html` — the release notes, rendered in the browser from `releases.json`.
- `datenschutz.html`, `datenschutz.en.html` — the privacy policy.
- `landing.css`, `landing.js` — the landing page's styles and its animated scenes
  (vanilla JS, no library).
- `docs.css` — the look of the docs and the recipes, in the landing page's design:
  same colours, fonts, light/dark and top bar. Colours the TUI uses are classes
  (`tc-red`, `tc-yellow`, …), not inline styles, so they stay readable on paper in
  both modes and switch to terminal colours inside a command block.
- `fonts/` — `fonts.css` plus Archivo and JetBrains Mono (latin + latin-ext, woff2)
  with their SIL OFL texts. Served from here on purpose: no request to Google,
  nothing for the privacy page to declare.
- `og-image.png` — the 1200×630 link-preview card (`og:image`, `twitter:image`),
  generated by `og-gen.py` from the site's own fonts and colours.
- `*.gif` — the terminal demos, generated by `demo-gen.py`, `portforward-gen.py`,
  `ui-gen.py`, `report-gen.py` and `contexts-gen.py`.
- `sitemap.xml`, `robots.txt` — for search engines; `<lastmod>` is set at build time.
- `nginx.conf` — listens on 8080; gzip, security headers, `/healthz`, the clean
  URLs (`/docs`, `/examples`, `/docker-swarm-exec-alternatives`, `/releases`,
  `/datenschutz`), `/compare` → the comparison, and the redirects of
  removed pages.
- `Dockerfile` — `nginx-unprivileged` (non-root); bakes `VERSION`, the sitemap
  date and `releases.json` in.
- `deploy/site-stack.yml` — Swarm stack (Traefik labels, gateway net, non-root +
  read-only + cap-drop).
- `proposals/` — design drafts; not part of the image.

The pages load Umami (self-hosted at `stats.cloud-surfers.net`) for page views
and link clicks (`data-umami-event`) — no session recording; the privacy policy
describes it. The landing page says so next to "no telemetry", which is about the
binary.
