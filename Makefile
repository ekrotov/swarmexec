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

# Tool versions (buf, protoc plugins, staticcheck, govulncheck, scip-go) live
# in tools.mk, which also keys CI's tool cache.
include tools.mk

# A tool is installed once per (version, Go version): the stamp file records
# what is in $(GOBIN), so a second `make tools` / `make lint` does not rebuild
# it, while a version bump in tools.mk or a new Go toolchain does.
GOVERSION  := $(shell go env GOVERSION)
TOOLSTAMPS := $(GOBIN)/.swarmexec-tools

# $(call install-tool,binary,package,version)
define install-tool
	@stamp="$(TOOLSTAMPS)/$(1)@$(3)-$(GOVERSION)"; \
	if [ -x "$(GOBIN)/$(1)" ] && [ -f "$$stamp" ]; then \
		echo "$(1) $(3) already installed ($(GOVERSION))"; \
	else \
		echo "go install $(2)@$(3)"; \
		go install $(2)@$(3) && mkdir -p "$(TOOLSTAMPS)" && touch "$$stamp"; \
	fi
endef

# Minimum total test coverage in percent (generated internal/pb excluded).
# Set just below the measured value; raise it as coverage grows, never lower it.
COVER_MIN  ?= 81.2

# Git ref whose .proto `proto-check` compares against for breaking changes.
BUF_AGAINST ?= origin/main

.PHONY: all fmt-check cli-docs cli-docs-check tools ci-tools tool-gocyclo cyclo mod-check tool-buf tool-protoc-gen-go tool-protoc-gen-go-grpc tool-staticcheck tool-govulncheck tool-scip-go generate check-generate proto-check build agent agent-image build-all test cover fuzz vet lint release clean

all: generate build-all

## build-all: build both the cli and the agent
build-all: build agent

## tools: install codegen plugins (buf brings its own compiler; no protoc needed)
tools: tool-buf tool-protoc-gen-go tool-protoc-gen-go-grpc

## ci-tools: install every pinned tool at once (CI installs each where it is used)
ci-tools: tools tool-staticcheck tool-gocyclo tool-govulncheck tool-scip-go

tool-buf:
	$(call install-tool,buf,github.com/bufbuild/buf/cmd/buf,$(BUF_VERSION))
tool-protoc-gen-go:
	$(call install-tool,protoc-gen-go,google.golang.org/protobuf/cmd/protoc-gen-go,$(PROTOC_GEN_GO_VERSION))
tool-protoc-gen-go-grpc:
	$(call install-tool,protoc-gen-go-grpc,google.golang.org/grpc/cmd/protoc-gen-go-grpc,$(PROTOC_GEN_GO_GRPC_VERSION))
tool-staticcheck:
	$(call install-tool,staticcheck,honnef.co/go/tools/cmd/staticcheck,$(STATICCHECK_VERSION))
tool-gocyclo:
	$(call install-tool,gocyclo,github.com/fzipp/gocyclo/cmd/gocyclo,$(GOCYCLO_VERSION))
tool-govulncheck:
	$(call install-tool,govulncheck,golang.org/x/vuln/cmd/govulncheck,$(GOVULNCHECK_VERSION))
tool-scip-go:
	$(call install-tool,scip-go,github.com/scip-code/scip-go/cmd/scip-go,$(SCIP_GO_VERSION))

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

## cli-docs: regenerate the site's CLI reference (site/docs/cli.html) from the command tree
# The page is the help text of every command and flag, rendered by
# client/cmd/gen-cli-docs; run it after changing a command and commit the page.
cli-docs:
	go run ./client/cmd/gen-cli-docs -site site -o site/docs/cli.html

## cli-docs-check: fail when site/docs/cli.html is not what the command tree renders (CI: test)
cli-docs-check: cli-docs
	@git diff --exit-code -- site/docs/cli.html site/docs/configuration.html || { \
		echo "site/docs/cli.html or configuration.html is out of date with the commands / exit-code table: run 'make cli-docs' and commit it"; exit 1; }

## test / vet
test:
	go test ./...

vet:
	go vet ./...

## cover: race tests with a coverage profile; fails below COVER_MIN percent or a per-file floor in ci/cover-floors.txt (CI: test)
# -count=1: always run the tests. CI keeps GOCACHE between pipelines, and a
# gate that reports "(cached)" results did not test this commit.
cover:
	go test -race -shuffle=on -count=1 -covermode=atomic -coverprofile=coverage.raw ./...
	grep -v '^swarmexec/internal/pb/' coverage.raw > coverage.out
	@go tool cover -func=coverage.out | awk -v min=$(COVER_MIN) '/^total:/ { \
		sub("%", "", $$3); printf "total coverage %s%% (minimum %s%%)\n", $$3, min; \
		if ($$3 + 0 < min + 0) { print "coverage is below the minimum"; exit 1 } }'
	@sh ci/cover-floors.sh coverage.out

## fuzz: run every fuzz target for FUZZTIME each (not in CI: `go test` already replays the seeds and any saved testdata/fuzz corpus)
# An input that breaks one is written to the package's testdata/fuzz/ — commit
# it, and it is a regression test in every later run.
FUZZTIME ?= 15s
FUZZ_TARGETS = \
	./client/internal/logfmt:FuzzFormatsParse \
	./client/internal/resolve:FuzzResolveTarget \
	./client/internal/stackfile:FuzzLoadComposeFile \
	./client/internal/swarmops:FuzzServiceEditParsers \
	./client/internal/cli:FuzzParseEnvText \
	./client/internal/cli:FuzzPortSpecs \
	./internal/forwardmux:FuzzRead \
	./internal/policy:FuzzParse \
	./internal/prunesched:FuzzParseLabels
fuzz:
	@for t in $(FUZZ_TARGETS); do \
		pkg=$${t%%:*}; fn=$${t##*:}; echo "== $$fn ($$pkg)"; \
		go test $$pkg -run '^$$' -fuzz "^$$fn\$$" -fuzztime $(FUZZTIME) || exit 1; \
	done

## lint: gofmt, staticcheck at the pinned version, and the complexity gate (CI: lint)
lint: fmt-check tool-staticcheck cyclo
	$(GOBIN)/staticcheck ./...

## fmt-check: fail when any Go file is not gofmt'd (CI: lint + test)
#
# A make target rather than a block of shell in .gitlab-ci.yml, because that is
# where it was: `make lint vet cover` passed locally while CI failed on a
# trailing blank line, since gofmt ran only inside the test job's script. One
# definition, both callers.
#
# The three Go roots are assigned first so a gofmt failure (a missing
# directory, say) fails the target instead of vanishing on stderr.
fmt-check:
	@unformatted="$$(gofmt -l agent client internal)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt: the following files need formatting:"; echo "$$unformatted"; \
		echo "run: gofmt -w $$unformatted"; exit 1; \
	fi

## cyclo: fail on any non-test function with cyclomatic complexity over 15 (CI: lint)
cyclo: tool-gocyclo
	@out="$$($(GOBIN)/gocyclo -over 15 $$(git ls-files '*.go' | grep -v '_test\.go$$' | grep -v '^internal/pb/'))"; \
	if [ -n "$$out" ]; then echo "functions over complexity 15:"; echo "$$out"; exit 1; fi

## mod-check: fail when go.mod/go.sum are not what `go mod tidy` writes (CI: test)
mod-check:
	go mod tidy -diff

clean:
	rm -rf bin dist coverage.raw coverage.out
