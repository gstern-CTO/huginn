BINARY := huginn
PKG    := ./cmd/huginn

.PHONY: all build test lint fmt vet integration cover clean install

all: lint test build

build:
	go build -o bin/$(BINARY) $(PKG)

install:
	go install $(PKG)

test:
	go test ./...

# Live GitHub tests. Requires GITHUB_TOKEN; Databricks and LSP cases skip
# when their prerequisites are absent.
integration:
	go test -tags=integration -v ./internal/tools/

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "coverage written to coverage.html"

fmt:
	gofmt -w .

vet:
	go vet ./...

lint: vet
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

clean:
	rm -rf bin coverage.out coverage.html
