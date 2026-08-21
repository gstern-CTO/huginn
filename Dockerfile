# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first: this layer is rebuilt only when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# The release workflow passes the git tag here so the binary reports its real
# version. Left unset for a local build, which keeps the in-source default.
ARG VERSION=""

# A static binary so the runtime stage can stay minimal.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w ${VERSION:+-X github.com/gstern-CTO/huginn/internal/tools.ServerVersion=$VERSION}" \
        -o /out/huginn ./cmd/huginn

# ---------------------------------------------------------------------------
# Runtime (default): small, with ripgrep for local search.
#
# No language server is installed here. That is deliberate: lsp_navigate
# degrades to its ripgrep symbol fallback and reports the install command,
# which is the designed behaviour rather than a failure. Use the runtime-go
# target below when exact Go navigation matters.
# ---------------------------------------------------------------------------
FROM alpine:3.21 AS runtime

RUN apk add --no-cache \
        ripgrep \
        git \
        ca-certificates \
    && addgroup -g 65532 -S huginn \
    && adduser -u 65532 -S -G huginn -h /home/huginn huginn \
    && mkdir -p /var/cache/huginn \
    && chown -R huginn:huginn /var/cache/huginn

COPY --from=build /out/huginn /usr/local/bin/huginn

USER huginn:huginn
WORKDIR /home/huginn

# Sensible container defaults. Everything is overridable at run time.
ENV ENABLE_LOCAL=true \
    WORKSPACE_ROOT=/workspace \
    CACHE_DIR=/var/cache/huginn \
    METRICS_ENABLED=false

# stdout is the MCP transport, so nothing may print to it but protocol frames.
ENTRYPOINT ["/usr/local/bin/huginn"]

# ---------------------------------------------------------------------------
# Runtime with Go language server.
#
# gopls needs the Go toolchain alongside it to resolve a module, so this stage
# keeps the full Go image. It is substantially larger; build it only when you
# want real go-to-definition rather than the ripgrep fallback.
# ---------------------------------------------------------------------------
FROM golang:1.26-alpine AS runtime-go

RUN apk add --no-cache ripgrep git ca-certificates \
    && go install golang.org/x/tools/gopls@latest \
    && mv /go/bin/gopls /usr/local/bin/gopls \
    && addgroup -g 65532 -S huginn \
    && adduser -u 65532 -S -G huginn -h /home/huginn huginn \
    && mkdir -p /var/cache/huginn /home/huginn/.cache \
    && chown -R huginn:huginn /var/cache/huginn /home/huginn

COPY --from=build /out/huginn /usr/local/bin/huginn

USER huginn:huginn
WORKDIR /home/huginn

ENV ENABLE_LOCAL=true \
    WORKSPACE_ROOT=/workspace \
    CACHE_DIR=/var/cache/huginn \
    METRICS_ENABLED=false \
    GOFLAGS=-mod=mod

ENTRYPOINT ["/usr/local/bin/huginn"]
