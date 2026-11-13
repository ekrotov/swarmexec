# swarmexec — build & codegen
#
# Single Go module (module swarmexec) shared by the cli and the agent. The
# generated proto code lives in internal/pb and is the contract surface both
# components compile against (proto/swarmexec.proto; semantics in CONTRACT.md).

GOBIN      := $(shell go env GOPATH)/bin
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PKG        := swarmexec/client/cmd/swarmexec
LDFLAGS    := -s -w -X main.version=$(VERSION)

# The agent keeps its version in a package (not main), so it has its own ldflags.
AGENT_LDFLAGS := -s -w -X swarmexec/agent/internal/version.Version=$(VERSION)
AGENT_IMAGE   ?= swarmexec-agent:$(VERSION)

# Release platforms of the cli — the one list (REQUIREMENTS-client §2 asks
# for at least linux/amd64, linux/arm64, darwin/arm64). CI's build-cli runs
# `make release`, and ci/verify-release.sh checks for exactly these binaries.
PLATFORMS  := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

# staticcheck 2026.2.2, pinned like GO_IMAGE: bump deliberately.
STATICCHECK_VERSION := v0.8.2

# Minimum total test coverage in percent (generated internal/pb excluded).
# Set just below the measured value; raise it as coverage grows, never lower it.
COVER_MIN  ?= 50.0

# Git ref whose .proto `proto-check` compares against for breaking changes.
BUF_AGAINST ?= origin/main

.PHONY: all tools generate check-generate proto-check build agent agent-image build-all test cover vet lint release clean

all: generate build-all

## build-all: build both the cli and the agent
build-all: build agent

## tools: install codegen plugins (buf brings its own compiler; no protoc needed)
tools:
	go install github.com/bufbuild/buf/cmd/buf@v1.45.0
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

## generate: regenerate internal/pb from proto/swarmexec.proto
generate:
	PATH="$(GOBIN):$$PATH" buf generate

## check-generate: fail when internal/pb does not match proto/swarmexec.proto
check-generate: generate
	@git diff --exit-code -- internal/pb || { echo "internal/pb is stale: run 'make generate' and commit"; exit 1; }
	@test -z "$$(git status --porcelain -- internal/pb)" || { git status --porcelain -- internal/pb; echo "untracked generated files in internal/pb"; exit 1; }

## proto-check: buf lint, plus buf breaking against the .proto at BUF_AGAINST (CI: generate-check)
proto-check:
	PATH="$(GOBIN):$$PATH" buf lint
	@tmp="$$(mktemp -d)"; \
	git archive -o "$$tmp/against.tar" "$(BUF_AGAINST)" proto buf.yaml || { rm -rf "$$tmp"; exit 1; }; \
	tar -xf "$$tmp/against.tar" -C "$$tmp" && rm "$$tmp/against.tar" || { rm -rf "$$tmp"; exit 1; }; \
	echo "buf breaking --against $(BUF_AGAINST)"; \
	PATH="$(GOBIN):$$PATH" buf breaking --against "$$tmp"; rc=$$?; rm -rf "$$tmp"; exit $$rc

## build: build the cli for the host platform into ./bin
build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/swarmexec ./client/cmd/swarmexec

## agent: build the agent for the host platform into ./bin
agent:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(AGENT_LDFLAGS)' -o bin/agent ./agent/cmd/agent

## agent-image: build the agent runtime image (context = repo root)
agent-image:
	docker build -f agent/Dockerfile --build-arg VERSION=$(VERSION) -t $(AGENT_IMAGE) .

## release: static cli binaries for all PLATFORMS + dist/SHA256SUMS (CI: build-cli)
release:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; \
		[ "$$os" = "windows" ] && ext=".exe"; \
		echo "  -> dist/swarmexec-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/swarmexec-$$os-$$arch$$ext ./client/cmd/swarmexec || exit 1; \
	done
	cd dist && sha256sum swarmexec-* > SHA256SUMS
	@ls -la dist

## test / vet
test:
	go test ./...

vet:
	go vet ./...

## cover: race tests with a coverage profile; fails below COVER_MIN percent (CI: test)
cover:
	go test -race -covermode=atomic -coverprofile=coverage.raw ./...
	grep -v '^swarmexec/internal/pb/' coverage.raw > coverage.out
	@go tool cover -func=coverage.out | awk -v min=$(COVER_MIN) '/^total:/ { \
		sub("%", "", $$3); printf "total coverage %s%% (minimum %s%%)\n", $$3, min; \
		if ($$3 + 0 < min + 0) { print "coverage is below the minimum"; exit 1 } }'

## lint: staticcheck at the pinned version (CI: lint)
lint:
	go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

clean:
	rm -rf bin dist coverage.raw coverage.out
