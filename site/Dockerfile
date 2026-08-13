# syntax=docker/dockerfile:1
#
# Static landing page for swarmexec (swarm-exec.logle.io), served by nginx as a
# NON-ROOT user (nginxinc/nginx-unprivileged: runs as uid 101, listens on 8080).
# Build from the repo root:
#   docker build -f site/Dockerfile --build-arg VERSION=v1.2.3 -t swarmexec-site .
#
# VERSION is baked into the page at build time; the release links themselves are
# GitLab "latest" permalinks, so they stay current even between rebuilds.
FROM nginxinc/nginx-unprivileged:1.27-alpine

ARG VERSION=dev

# Root only for the build steps; the base image drops back to the nginx user.
USER root
COPY site/nginx.conf /etc/nginx/conf.d/default.conf
# Glob covers every language variant (index.de.html, docs.fr.html, datenschutz.es.html, …).
COPY site/index*.html site/docs*.html site/datenschutz*.html site/robots.txt \
     site/favicon.svg site/favicon.ico site/apple-touch-icon.png site/demo.gif /usr/share/nginx/html/
# index*.html and docs*.html carry the __VERSION__ placeholder (in every language); datenschutz*.html does not.
RUN sed -i "s/__VERSION__/${VERSION}/g" /usr/share/nginx/html/index*.html /usr/share/nginx/html/docs*.html \
 && chown -R nginx:nginx /usr/share/nginx/html \
 && chown nginx:nginx /etc/nginx/conf.d/default.conf
USER nginx

EXPOSE 8080
# Use 127.0.0.1, NOT localhost: nginx binds IPv4 only (`listen 8080;`), while
# localhost also resolves to ::1, and busybox wget tries ::1 without falling
# back — so `localhost` yields "connection refused" and the check never passes.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1
