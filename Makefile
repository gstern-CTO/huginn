BINARY  := huginn
PKG     := ./cmd/huginn
COMPOSE := docker compose

.PHONY: all build test lint fmt vet integration cover clean install \
        docker-build docker-build-go run-local run-observed docker-smoke \
        docker-cache-clean docker-clean smoke help

all: lint test build

## help: list the available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'

# ---------------------------------------------------------------------------
# Local Go toolchain
# ---------------------------------------------------------------------------

## build: compile the binary into ./bin
build:
	go build -o bin/$(BINARY) $(PKG)

## install: install the binary into GOPATH/bin
install:
	go install $(PKG)

## test: run the hermetic unit tests
test:
	go test ./...

## integration: run live GitHub tests (needs GITHUB_TOKEN)
integration:
	go test -tags=integration -v ./internal/tools/

## cover: write an HTML coverage report
cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "coverage written to coverage.html"

fmt:
	gofmt -w .

vet:
	go vet ./...

## lint: gofmt check plus go vet
lint: vet
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

# ---------------------------------------------------------------------------
# Docker
#
# Huginn speaks MCP over stdio, so there is no server to leave running in the
# background. Every target below starts a container for exactly one session
# and removes it afterwards, which is how an MCP client drives it too.
# ---------------------------------------------------------------------------

## docker-build: build the container image (ripgrep, no language server)
docker-build:
	$(COMPOSE) --profile mcp build

## docker-build-go: build the larger image that includes gopls
docker-build-go:
	HUGINN_TARGET=runtime-go $(COMPOSE) --profile mcp build

## run-local: start one containerised MCP session on stdio
##            HUGINN_WORKSPACE=/path/to/code make run-local
run-local:
	@echo "Huginn MCP server on stdio. Workspace: $${HUGINN_WORKSPACE:-$$PWD} (read-only)." >&2
	@echo "Type JSON-RPC requests, one per line. Ctrl-D ends the session." >&2
	@echo "Try: {\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2024-11-05\",\"capabilities\":{},\"clientInfo\":{\"name\":\"cli\",\"version\":\"1\"}}}" >&2
	@$(COMPOSE) --profile mcp run --rm -T huginn

## run-observed: one session with Prometheus metrics on 127.0.0.1:9090
run-observed:
	$(COMPOSE) --profile observe up --abort-on-container-exit

## docker-smoke: drive a real handshake through the container and list tools
docker-smoke:
	@scripts/mcp-smoke.sh $(COMPOSE) --profile mcp run --rm -T huginn

## smoke: same handshake against a locally built binary
smoke: build
	@scripts/mcp-smoke.sh ./bin/$(BINARY)

## docker-cache-clean: drop the persistent GitHub response cache volume
docker-cache-clean:
	-$(COMPOSE) --profile mcp --profile observe down --volumes --remove-orphans

## docker-clean: remove containers, the cache volume and the image
docker-clean: docker-cache-clean
	-docker image rm huginn:$${HUGINN_TAG:-local}

clean:
	rm -rf bin coverage.out coverage.html
