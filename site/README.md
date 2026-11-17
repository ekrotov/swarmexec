# swarmexec website

A small static site for the project, served at **https://swarm-exec.cloud-surfers.net**
by nginx (non-root, port 8080) behind Traefik.

| URL | File | What |
|---|---|---|
| `/` | `index.html` | landing page |
| `/docs` | `docs.html` | documentation index: how it works, and a list of every docs page |
| `/docs/<topic>` | `docs/<topic>.html` | one docs page per topic — `install`, `exec`, `logs`, `port-forward`, `stack`, `security-scan`, `secrets`, `volumes`, `mcp`, `agents`, `authentication`, `contexts`, `configuration`, `tui`, `tui-services` (an overview of `tui-services-tree`, `tui-services-security`, `tui-services-inspect`, `tui-services-edit`), `tui-tabs`, `tui-nodes`, `cli` (generated, see below), `parity` |
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

The build bakes the version into every page (the `__VERSION__` placeholder,
also used as `?v=` on stylesheets and scripts),
stamps the build date into `sitemap.xml` (`<lastmod>`), and fetches the tag list
from the GitLab API into `releases.json` for the release-notes page.

## The top bar

Every page has the same top bar: the swarmexec mark (home), Docs, Examples,
Compare, Releases, GitHub and the Install button. The markup lives once, in
[`partials/nav.html`](partials/nav.html); each page carries a verbatim copy in
which only the item for the page itself has `aria-current` (`page`; `true` on
the docs topic pages, which are inside Docs). It is styled only by `nav.css`,
which every page links. To change it, edit the partial (and `nav.css`), then

```sh
sh site/nav.sh write   # copy the partial into every page
sh site/nav.sh check   # what CI runs (site-check job): fails on any difference
```

A new page goes into the table at the top of `nav.sh`; `check` fails for a
served page that is missing from it.

The docs page list works the same way. Every `docs/<topic>.html` carries a
verbatim copy of [`partials/docnav.html`](partials/docnav.html) (the
`<aside class="docside">`: "All docs pages" and the search box) in which its
own entry has `aria-current="page"`. It sits at the foot of the page on narrow
screens and becomes a left sidebar from 980px (`docs.css`). A new docs page
goes into the partial; `nav.sh write` copies it everywhere, and `check` fails
on any difference, and on an entry whose page does not exist.

## How a change gets live

From [`.gitlab-ci.yml`](../.gitlab-ci.yml):

- **Merge to `main` touching `site/`** — `site-image` builds the image with
  `VERSION=latest` and pushes `:latest` and `:main` to
  `registry.logle.io/cs-public/swarm-remote-exec/site`. Nothing in this repository
  deploys it.
- **Release tag** (`vX.Y.Z`) — `site-image` pushes `:vX.Y.Z`, `:X.Y.Z` and, for a
  stable release, `:latest`; then `deploy-site` triggers the pipeline of
  `infrastruktur/swar-exec-site`, which renders the stack with that version and
  deploys it. The `verify-site` job (`ci/verify-release.sh` with
  `VERIFY_ONLY=site`) then checks that every replica serves the release and
  that its notes list it. It is non-blocking: a stalled rollout (a task stuck
  in "Preparing" after one failed image pull) shows as a warning, and its log
  names the fix, `docker service update --force swarm-exec-site_site`.

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
- `docs.html` — the documentation index (how it works, and every docs page with a
  one-line summary). It also carries a small script that forwards the fragments of
  the old one-page docs (`/docs#exec`, `/docs#tui-secrets`, …) to the page that holds
  them now; **when an id moves to another page, update that table**.
- `docs/*.html` — the documentation, one topic per page so each can answer its own
  question: own title, description, canonical URL, breadcrumb, previous/next and the
  full page list at the end. Headings keep the ids of the old single page, so
  `/docs/exec#exec`, `/docs/tui-tabs#tui-secrets` and so on are stable deep links.
  Links between pages are absolute (`/docs/logs#logs`, `/examples#forward-db`),
  because a relative link resolves differently under `/docs/`. A new page needs its
  entry in every page's list and prev/next, the index, and `sitemap.xml`.
  `docs/tui-services.html` is the overview of the Stacks/Services tab; its detail
  moved to `tui-services-tree`, `-security`, `-inspect` and `-edit`, and like the
  index it carries a small script that forwards a moved fragment
  (`/docs/tui-services#env-editor`) to the page that holds it now — keep that table
  in step too.
- `docs/cli.html` — the CLI reference, **generated**: `make cli-docs` renders every
  command, subcommand and visible flag from the cobra tree
  (`client/cmd/gen-cli-docs`, `client/internal/clidocs`), with the top bar and
  the docs list taken from `partials/`. Do not edit it by hand; change the
  command's help text and regenerate. CI (`test`: `make cli-docs-check`) fails
  when the checked-in page differs from what the tree renders.
- `docs/parity.html` — CLI · TUI · MCP side by side, one row per capability. Hand-written
  and checked against `client/internal/cli` (commands, `keymap.go`, the menus in
  `ui_containers.go` / `ui_inspect.go`) and `client/internal/mcp/tools.go`. The rule it
  records: whatever the TUI changes has a CLI twin or is listed there as TUI-only with
  its reason. **A new command, TUI action or MCP tool gets its row.**
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
- `docs.css` — the look of every page but the landing page (docs, recipes, comparison,
  release notes, privacy), in the landing page's design:
  same colours, fonts, light/dark and top bar. Colours the TUI uses are classes
  (`tc-red`, `tc-yellow`, …), not inline styles, so they stay readable on paper in
  both modes and switch to terminal colours inside a command block.
- `copy.js` — the copy button on a command block carrying `data-copy` (the install
  one-liner in the landing page's hero and on `/docs/install`). It creates the button
  only when the Clipboard API is there; CSS reserves its room either way.
- `docsearch.js` — the docs search box (in `partials/docnav.html` and on the index).
  The index is built by [pagefind](https://pagefind.app) in the image's `search`
  stage (pinned release, checked against a sha256 in the Dockerfile) over `docs.html`
  and `docs/*.html` only, and served from `/pagefind/`: no search service, no
  request elsewhere. The form is `hidden` until the script runs, so without
  JavaScript, or opened from the repository without an index, the page list is the
  way around. `/docs?q=…` opens with a search done. To bump pagefind, change
  `PAGEFIND_VERSION` and both checksums (the `.sha256` files on its release).
- `fonts/` — `fonts.css` plus Archivo and JetBrains Mono (latin + latin-ext, woff2)
  with their SIL OFL texts. Served from here on purpose: no request to Google,
  nothing for the privacy page to declare.
- `og-image.png` — the 1200×630 link-preview card (`og:image`, `twitter:image`),
  generated by `og-gen.py` from the site's own fonts and colours.
- `*.gif` — the terminal demos, generated by `demo-gen.py`, `portforward-gen.py`,
  `ui-gen.py`, `report-gen.py` and `contexts-gen.py`.
- `sitemap.xml`, `robots.txt` — for search engines; `<lastmod>` is set at build time.
- `nginx.conf` — listens on 8080; gzip, security headers, `/healthz`, the clean
  URLs (`/docs`, `/docs/<topic>`, `/examples`, `/docker-swarm-exec-alternatives`,
  `/releases`, `/datenschutz`), `/compare` → the comparison, `/docs/<topic>.html`
  and `/docs/` → their clean URL, and the redirects of
  removed pages.
- `Dockerfile` — `nginx-unprivileged` (non-root); bakes `VERSION`, the sitemap
  date, `releases.json` and the docs search index (`/pagefind/`) in.
- `deploy/site-stack.yml` — Swarm stack (Traefik labels, gateway net, non-root +
  read-only + cap-drop).
- `proposals/` — design drafts; not part of the image.

The pages load Umami (self-hosted at `stats.cloud-surfers.net`) for page views
and link clicks (`data-umami-event`) — no session recording; the privacy policy
describes it. The landing page says so next to "no telemetry", which is about the
binary.
