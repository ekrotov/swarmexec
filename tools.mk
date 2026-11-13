# Pinned versions of the Go tools the build and CI use. Included by the
# Makefile; bump deliberately, like GO_IMAGE.
#
# This file is ALSO the key of CI's tool cache (.gitlab-ci.yml, .cache-tools):
# changing a version here starts a fresh cache, so keep everything that is not
# a tool version out of it.

# codegen (make tools / generate)
BUF_VERSION                := v1.45.0
PROTOC_GEN_GO_VERSION      := v1.34.2
PROTOC_GEN_GO_GRPC_VERSION := v1.5.1

# staticcheck 2026.2.2 (make lint)
STATICCHECK_VERSION        := v0.8.2

# CI only: govulncheck (the vulncheck gate parses its JSON output) and the
# SCIP code-navigation indexer
GOVULNCHECK_VERSION        := v1.9.0
SCIP_GO_VERSION            := v0.2.9
