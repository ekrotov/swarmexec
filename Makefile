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

# Operator platforms (REQUIREMENTS §2).
PLATFORMS  := linux/amd64 linux/arm64 darwin/arm64

.PHONY: all tools generate check-generate build agent agent-image build-all test vet lint release clean

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

## build: build the cli for the host platform into ./bin
build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/swarmexec ./client/cmd/swarmexec

## agent: build the agent for the host platform into ./bin
agent:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(AGENT_LDFLAGS)' -o bin/agent ./agent/cmd/agent

## agent-image: build the agent runtime image (context = repo root)
agent-image:
	docker build -f agent/Dockerfile --build-arg VERSION=$(VERSION) -t $(AGENT_IMAGE) .

## release: static cross-compiled binaries for all operator platforms
release:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/swarmexec-$$os-$$arch ./client/cmd/swarmexec || exit 1; \
	done
	@ls -la dist

## test / vet
test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin dist
