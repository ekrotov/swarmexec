# syntax=docker/dockerfile:1
#
# Static landing page for swarmexec (swarm-exec.logle.io), served by nginx.
# Build from this directory:
#   docker build -f site/Dockerfile --build-arg VERSION=v1.2.3 -t swarmexec-site .
#
# VERSION is baked into the page at build time; the release links themselves are
# GitLab "latest" permalinks, so they stay current even between rebuilds.
FROM nginx:1.27-alpine

ARG VERSION=dev

COPY site/nginx.conf /etc/nginx/conf.d/default.conf
COPY site/index.html /usr/share/nginx/html/index.html

# Bake the version into the page.
RUN sed -i "s/__VERSION__/${VERSION}/g" /usr/share/nginx/html/index.html

EXPOSE 80
HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
  CMD wget -qO- http://localhost/healthz >/dev/null 2>&1 || exit 1
