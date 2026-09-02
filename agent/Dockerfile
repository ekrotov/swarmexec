# syntax=docker/dockerfile:1
#
# Build from the REPOSITORY ROOT (the shared module), not from agent/:
#   docker build -f agent/Dockerfile -t swarmexec-agent:<ver> .
# The agent and cli share one Go module (module swarmexec) and the generated
# internal/pb, so the build context must include the whole repo.

# --- build stage: compile a static binary ---
FROM golang:1.27-alpine AS build
WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

# Build the agent command from the shared module.
COPY . .
ARG VERSION=dev
ARG PROTO_VER=swarmexec/v1
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X swarmexec/agent/internal/version.Version=${VERSION} -X swarmexec/agent/internal/version.Protocol=${PROTO_VER}" \
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
ENTRYPOINT ["/agent"]
