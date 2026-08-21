#!/usr/bin/env bash
#
# huginn-docker.sh — register the containerised Huginn with Claude Code.
#
# Use this when you want Huginn without installing Go, ripgrep or a language
# server. Everything ships inside the image; Docker is the only prerequisite.
#
# Usage:
#   ./huginn-docker.sh --workspace ~/code --git-token ghp_...
#   ./huginn-docker.sh --workspace ~/code --no-token
#   ./huginn-docker.sh --workspace ~/code --tag 0.1.0
#
# Options:
#   --workspace PATH         Directory to expose to Huginn, mounted read-only.
#                            Required: the container has no other view of disk.
#   --git-token TOKEN        GitHub token. See the security note below.
#   --git-token-file PATH    Read the token from a file instead. Preferred.
#   --no-token               Register without GitHub access (6 of 11 tools).
#   --image REF              Image repository (default ghcr.io/gstern-cto/huginn).
#   --tag TAG                Image tag (default latest). Pin this in anger.
#   --name NAME              MCP server name (default huginn).
#   --scope local|user|project   Registration scope (default user).
#   --no-pull                Do not pull; use whatever image is already local.
#   --force                  Replace an existing registration without asking.
#   --dry-run                Print what would happen, change nothing.
#   -h, --help               This text.
#
# SECURITY NOTE on --git-token
#   A token on the command line is visible in your shell history and, briefly,
#   to anyone who can run `ps`. --git-token-file avoids both. Either way Claude
#   Code stores it in ~/.claude.json in plain text; that is how MCP environment
#   variables work. For public repositories a token with NO scopes is enough.

set -uo pipefail

IMAGE="ghcr.io/gstern-cto/huginn"
TAG="latest"
WORKSPACE=""
TOKEN=""
TOKEN_FILE=""
NO_TOKEN=0
NAME="huginn"
SCOPE="user"
NO_PULL=0
FORCE=0
DRY_RUN=0

if [ -t 1 ]; then
    R=$'\033[31m'; G=$'\033[32m'; Y=$'\033[33m'; DIM=$'\033[2m'; BOLD=$'\033[1m'; N=$'\033[0m'
else
    R=""; G=""; Y=""; DIM=""; BOLD=""; N=""
fi

die()  { printf '%s✘ %s%s\n' "$R" "$1" "$N" >&2; exit 1; }
ok()   { printf '%s✔%s %s\n' "$G" "$N" "$1"; }
warn() { printf '%s!%s %s\n' "$Y" "$N" "$1"; }
step() { printf '\n%s%s%s\n' "$BOLD" "$1" "$N"; }
usage() { sed -n '3,33p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }
have() { type -P "$1" >/dev/null 2>&1; }

while [ $# -gt 0 ]; do
    case "$1" in
        --workspace)       [ $# -ge 2 ] || die "--workspace needs a path"; WORKSPACE="$2"; shift 2 ;;
        --workspace=*)     WORKSPACE="${1#*=}"; shift ;;
        --git-token)       [ $# -ge 2 ] || die "--git-token needs a value"; TOKEN="$2"; shift 2 ;;
        --git-token=*)     TOKEN="${1#*=}"; shift ;;
        --git-token-file)  [ $# -ge 2 ] || die "--git-token-file needs a path"; TOKEN_FILE="$2"; shift 2 ;;
        --git-token-file=*) TOKEN_FILE="${1#*=}"; shift ;;
        --no-token)        NO_TOKEN=1; shift ;;
        --image)           [ $# -ge 2 ] || die "--image needs a value"; IMAGE="$2"; shift 2 ;;
        --image=*)         IMAGE="${1#*=}"; shift ;;
        --tag)             [ $# -ge 2 ] || die "--tag needs a value"; TAG="$2"; shift 2 ;;
        --tag=*)           TAG="${1#*=}"; shift ;;
        --name)            [ $# -ge 2 ] || die "--name needs a value"; NAME="$2"; shift 2 ;;
        --name=*)          NAME="${1#*=}"; shift ;;
        --scope)           [ $# -ge 2 ] || die "--scope needs a value"; SCOPE="$2"; shift 2 ;;
        --scope=*)         SCOPE="${1#*=}"; shift ;;
        --no-pull)         NO_PULL=1; shift ;;
        --force)           FORCE=1; shift ;;
        --dry-run)         DRY_RUN=1; shift ;;
        -h|--help)         usage 0 ;;
        *)                 printf '%sunknown option: %s%s\n\n' "$R" "$1" "$N" >&2; usage 2 ;;
    esac
done

case "$SCOPE" in local|user|project) ;; *) die "--scope must be local, user or project (got '$SCOPE')" ;; esac

IMAGE_REF="${IMAGE}:${TAG}"

# ---------------------------------------------------------------------------
# Token
# ---------------------------------------------------------------------------

if [ -n "$TOKEN_FILE" ]; then
    [ -n "$TOKEN" ] && die "pass either --git-token or --git-token-file, not both"
    [ -r "$TOKEN_FILE" ] || die "cannot read token file: $TOKEN_FILE"
    TOKEN="$(tr -d '[:space:]' < "$TOKEN_FILE")"
    [ -n "$TOKEN" ] || die "token file $TOKEN_FILE is empty"
fi

if [ "$NO_TOKEN" = 1 ]; then
    [ -n "$TOKEN" ] && die "--no-token conflicts with a supplied token"
    TOKEN=""
elif [ -z "$TOKEN" ]; then
    for var in OCTOCODE_TOKEN GH_TOKEN GITHUB_TOKEN; do
        if [ -n "${!var:-}" ]; then TOKEN="${!var}"; warn "using \$$var from the environment"; break; fi
    done
fi

if [ -z "$TOKEN" ] && [ "$NO_TOKEN" = 0 ]; then
    printf 'No GitHub token supplied.\n'
    printf '  Pass --git-token ghp_... or --git-token-file PATH, or use --no-token.\n'
    printf '  Create one at https://github.com/settings/tokens — no scopes needed for public repos.\n'
    exit 2
fi

# ---------------------------------------------------------------------------
# Prerequisites
# ---------------------------------------------------------------------------

step "Checking prerequisites"

have docker || die "Docker is not installed. See https://docs.docker.com/get-docker/"
docker info >/dev/null 2>&1 || die "Docker is installed but the daemon is not reachable. Is it running?"
ok "docker $(docker version --format '{{.Server.Version}}' 2>/dev/null)"

have claude || die "the Claude Code CLI is not on your PATH. See https://claude.com/claude-code"
ok "claude found at $(type -P claude)"

# The container's only view of disk is this mount, so it is not optional.
[ -n "$WORKSPACE" ] || die "--workspace is required: the container sees no files unless you mount them"
WORKSPACE_INPUT="$WORKSPACE"
WORKSPACE="$(cd "$WORKSPACE" 2>/dev/null && pwd)" || die "workspace directory does not exist: $WORKSPACE_INPUT"
[ -n "$WORKSPACE" ] && [ -d "$WORKSPACE" ] || die "workspace directory does not exist: $WORKSPACE_INPUT"
ok "workspace: $WORKSPACE (mounted read-only)"

# ---------------------------------------------------------------------------
# Image
# ---------------------------------------------------------------------------

step "Image"

if [ "$NO_PULL" = 1 ]; then
    docker image inspect "$IMAGE_REF" >/dev/null 2>&1 \
        || die "--no-pull given but $IMAGE_REF is not present locally"
    ok "using the local $IMAGE_REF"
elif [ "$DRY_RUN" = 1 ]; then
    printf '  %swould pull %s%s\n' "$DIM" "$IMAGE_REF" "$N"
else
    printf '  pulling %s\n' "$IMAGE_REF"
    if ! docker pull "$IMAGE_REF" 2>&1 | sed 's/^/  /'; then
        if docker image inspect "$IMAGE_REF" >/dev/null 2>&1; then
            warn "pull failed; falling back to the local copy of $IMAGE_REF"
        else
            die "could not pull $IMAGE_REF and no local copy exists"
        fi
    fi
fi

if [ "$DRY_RUN" = 0 ]; then
    IMAGE_VERSION="$(docker run --rm "$IMAGE_REF" --version 2>&1 | head -1)"
    ok "image reports: $IMAGE_VERSION"
fi

# ---------------------------------------------------------------------------
# Token check
# ---------------------------------------------------------------------------

if [ -n "$TOKEN" ] && have curl; then
    step "Verifying the GitHub token"
    LIMIT="$(curl -sS -m 15 -H "Authorization: Bearer $TOKEN" https://api.github.com/rate_limit 2>/dev/null \
             | grep -oE '"limit":[[:space:]]*[0-9]+' | head -1 | grep -oE '[0-9]+')"
    case "${LIMIT:-}" in
        ""|60) warn "GitHub is treating this token as anonymous (rate limit ${LIMIT:-unknown})."
               if [ "$FORCE" = 0 ]; then
                   printf '  Re-run with %s--force%s to register it anyway.\n' "$BOLD" "$N"; exit 1
               fi
               warn "--force given; registering it regardless" ;;
        *)     ok "token accepted — rate limit ${LIMIT}/hour" ;;
    esac
fi

# ---------------------------------------------------------------------------
# Register
# ---------------------------------------------------------------------------

step "Registering '$NAME' with Claude Code (scope: $SCOPE)"

# -i is required and -t must be absent: MCP is a byte stream and a TTY's line
# discipline would corrupt it. The workspace is read-only because the kernel
# enforcing that is a second layer under Huginn's own path guard, and the
# named volume keeps the response cache alive between sessions.
DOCKER_ARGS=(
    run --rm -i --init
    --mount "type=bind,src=${WORKSPACE},dst=/workspace,ro"
    --mount "type=volume,src=huginn-cache,dst=/var/cache/huginn"
    --read-only --tmpfs /tmp
    --cap-drop ALL --security-opt no-new-privileges
    -e ENABLE_LOCAL=true
    -e WORKSPACE_ROOT=/workspace
    -e METRICS_ENABLED=false
)
[ -n "$TOKEN" ] && DOCKER_ARGS+=(-e "GITHUB_TOKEN=${TOKEN}")
DOCKER_ARGS+=("$IMAGE_REF")

REDACTED=("${DOCKER_ARGS[@]}")
for i in "${!REDACTED[@]}"; do
    case "${REDACTED[$i]}" in GITHUB_TOKEN=*) REDACTED[$i]="GITHUB_TOKEN=***redacted***" ;; esac
done
printf '  %sclaude mcp add %s -s %s -- docker %s%s\n' "$DIM" "$NAME" "$SCOPE" "${REDACTED[*]}" "$N"

if [ "$DRY_RUN" = 1 ]; then
    printf '\n%s--dry-run: nothing was changed.%s\n\n' "$BOLD" "$N"
    exit 0
fi

if claude mcp list 2>/dev/null | grep -q "^${NAME}:"; then
    if [ "$FORCE" = 0 ]; then
        printf '\n  A server named %s'"'"'%s'"'"'%s is already registered. Replace it? [y/N] ' "$BOLD" "$NAME" "$N"
        read -r reply
        case "$reply" in [yY]*) ;; *) printf '  Left unchanged.\n\n'; exit 0 ;; esac
    fi
    claude mcp remove "$NAME" -s "$SCOPE" >/dev/null 2>&1 || claude mcp remove "$NAME" >/dev/null 2>&1 || true
    ok "removed the previous registration"
fi

claude mcp add "$NAME" -s "$SCOPE" -- docker "${DOCKER_ARGS[@]}" 2>&1 | sed 's/^/  /' \
    || die "registration failed"

# ---------------------------------------------------------------------------
# Verify
# ---------------------------------------------------------------------------

step "Verifying the registration"

STATUS="$(claude mcp list 2>/dev/null | grep "^${NAME}:" || true)"
[ -n "$STATUS" ] || die "'$NAME' does not appear in 'claude mcp list' after registering"

# For the container form the token is part of the command, so `claude mcp list`
# prints it. Never echo that verbatim.
SAFE_STATUS="$STATUS"
[ -n "$TOKEN" ] && SAFE_STATUS="${STATUS//$TOKEN/***redacted***}"

if printf '%s' "$STATUS" | grep -qi 'connected'; then
    ok "${NAME}: ✔ Connected"
else
    warn "$SAFE_STATUS"
    warn "registered, but the health check did not report Connected."
fi

printf '\n%s────────────────────────────────────────────────────────────%s\n' "$BOLD" "$N"
printf '%s✔ Huginn is registered, running in a container.%s\n\n' "$G" "$N"
printf '  image      %s\n' "$IMAGE_REF"
printf '  workspace  %s %s(read-only)%s\n' "$WORKSPACE" "$DIM" "$N"
printf '  github     %s\n' "$([ -n "$TOKEN" ] && echo 'configured — all 11 tools available' || echo 'not configured — 6 of 11 tools available')"
printf '  cache      docker volume "huginn-cache" %s(persists between sessions)%s\n' "$DIM" "$N"
if [ -n "$TOKEN" ]; then
    printf '\n%s!%s The token is part of the docker command, so %sclaude mcp list%s prints it\n' "$Y" "$N" "$BOLD" "$N"
    printf '  in clear text. That is unavoidable for the container form — the native\n'
    printf '  install (%s./huginn.sh%s) passes it as an env var and does not show it.\n' "$BOLD" "$N"
fi
printf '\n  %sRestart Claude Code%s for the tools to appear in a session.\n' "$BOLD" "$N"
printf '  %sOnly %s is visible to Huginn. Re-run with a different --workspace to change that.%s\n\n' "$DIM" "$WORKSPACE" "$N"
