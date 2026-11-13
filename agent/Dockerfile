# syntax=docker/dockerfile:1
#
# Build from the REPOSITORY ROOT (the shared module), not from agent/:
#   docker build -f agent/Dockerfile -t swarmexec-agent:<ver> .
# The agent and cli share one Go module (module swarmexec) and the generated
# internal/pb, so the build context must include the whole repo.

# --- build stage: compile a static binary ---
FROM golang:1.27.4-alpine AS build
WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

# Build the agent command from the shared module.
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X swarmexec/agent/internal/version.Version=${VERSION}" \
    -o /agent ./agent/cmd/agent

# --- runtime stage: minimal distroless (no shell, no docker CLI) ---
# The Docker socket is bind-mounted at runtime; the CLI is NOT shipped.
# Runs as root so the bind-mounted /var/run/docker.sock is writable (exec needs
# write). Mounting the socket already grants host-root-equivalent power, so this
# does not widen the trust boundary; the socket is never exposed to the network.
FROM gcr.io/distroless/static-debian12
COPY --from=build /agent /agent
# Apache-2.0 §4(a)/(d): the license and attribution notice travel with the image.
COPY LICENSE NOTICE /licenses/
EXPOSE 9443
# Container health: the probe is the agent binary itself (distroless has no
# shell to run anything else), exec form. It reads the agent's command line
# from /proc/1/cmdline, dials the agent's port on loopback and pings Docker.
# Being in the image, an image-only agent update turns it on. Timings mirror
# internal/deploy (Health*), which a test keeps in step.
HEALTHCHECK --interval=10s --timeout=10s --start-period=30s --retries=6 CMD ["/agent", "-healthcheck"]
ENTRYPOINT ["/agent"]
