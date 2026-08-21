#!/usr/bin/env bash
#
# huginn.sh — register Huginn as an MCP server with Claude Code.
#
# Usage:
#   ./huginn.sh --git-token ghp_xxxxxxxxxxxx
#   ./huginn.sh --git-token-file ~/.huginn-token --workspace ~/code
#   ./huginn.sh --no-token                       # local tools only
#
# Options:
#   --git-token TOKEN        GitHub token. See the security note below.
#   --git-token-file PATH    Read the token from a file instead. Preferred.
#   --no-token               Register without GitHub access.
#   --workspace PATH         Workspace root (default: the current directory).
#   --name NAME              MCP server name (default: huginn).
#   --scope local|user|project   Registration scope (default: user).
#   --metrics [PORT]         Enable Prometheus metrics (default port 9090).
#   --force                  Replace an existing registration without asking.
#   --dry-run                Print what would happen, change nothing.
#   -h, --help               This text.
#
# SECURITY NOTE on --git-token
#   A token on the command line is visible in your shell history and, briefly,
#   to anyone who can run `ps` on this machine. --git-token-file avoids both.
#   Either way Claude Code stores the token in ~/.claude.json in plain text;
#   that is how MCP environment variables work, not something this script
#   chooses. Use a token with the fewest scopes that does the job — for public
#   repositories a token with NO scopes still gets 5,000 requests/hour.

set -uo pipefail

TOKEN=""
TOKEN_FILE=""
NO_TOKEN=0
WORKSPACE=""
NAME="huginn"
SCOPE="user"
METRICS_ENABLED="false"
METRICS_PORT="9090"
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

usage() { sed -n '3,30p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------

while [ $# -gt 0 ]; do
    case "$1" in
        --git-token)
            [ $# -ge 2 ] || die "--git-token needs a value"
            TOKEN="$2"; shift 2 ;;
        --git-token=*)  TOKEN="${1#*=}"; shift ;;
        --git-token-file)
            [ $# -ge 2 ] || die "--git-token-file needs a path"
            TOKEN_FILE="$2"; shift 2 ;;
        --git-token-file=*) TOKEN_FILE="${1#*=}"; shift ;;
        --no-token)     NO_TOKEN=1; shift ;;
        --workspace)
            [ $# -ge 2 ] || die "--workspace needs a path"
            WORKSPACE="$2"; shift 2 ;;
        --workspace=*)  WORKSPACE="${1#*=}"; shift ;;
        --name)
            [ $# -ge 2 ] || die "--name needs a value"
            NAME="$2"; shift 2 ;;
        --name=*)       NAME="${1#*=}"; shift ;;
        --scope)
            [ $# -ge 2 ] || die "--scope needs a value"
            SCOPE="$2"; shift 2 ;;
        --scope=*)      SCOPE="${1#*=}"; shift ;;
        --metrics)
            METRICS_ENABLED="true"; shift
            # An optional port may follow, e.g. --metrics 9091
            if [ $# -ge 1 ] && printf '%s' "$1" | grep -qE '^[0-9]+$'; then METRICS_PORT="$1"; shift; fi ;;
        --metrics=*)    METRICS_ENABLED="true"; METRICS_PORT="${1#*=}"; shift ;;
        --force)        FORCE=1; shift ;;
        --dry-run)      DRY_RUN=1; shift ;;
        -h|--help)      usage 0 ;;
        *)              printf '%sunknown option: %s%s\n\n' "$R" "$1" "$N" >&2; usage 2 ;;
    esac
done

case "$SCOPE" in
    local|user|project) ;;
    *) die "--scope must be local, user or project (got '$SCOPE')" ;;
esac

# ---------------------------------------------------------------------------
# Resolve the token
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
    # Fall back to the environment, in the order Huginn itself resolves it.
    for var in OCTOCODE_TOKEN GH_TOKEN GITHUB_TOKEN; do
        if [ -n "${!var:-}" ]; then
            TOKEN="${!var}"
            warn "no --git-token given; using \$$var from the environment"
            break
        fi
    done
fi

if [ -z "$TOKEN" ] && [ "$NO_TOKEN" = 0 ]; then
    printf '%s\n' "No GitHub token supplied."
    printf '  Pass one with --git-token ghp_... or --git-token-file PATH,\n'
    printf '  or register without GitHub access using --no-token.\n\n'
    printf '  Create a token at https://github.com/settings/tokens\n'
    printf '  For public repositories only, a token with NO scopes is enough.\n'
    exit 2
fi

# A token that is not shaped like one is almost always a copy-paste accident.
if [ -n "$TOKEN" ] && ! printf '%s' "$TOKEN" | grep -qE '^(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})$'; then
    warn "the token does not look like a GitHub token (expected ghp_… or github_pat_…)"
    warn "continuing anyway — GitHub Enterprise tokens can differ"
fi

# ---------------------------------------------------------------------------
# Preconditions
# ---------------------------------------------------------------------------

step "Checking prerequisites"

have() { type -P "$1" >/dev/null 2>&1; }

have claude || die "the Claude Code CLI is not on your PATH. Install it from https://claude.com/claude-code"
ok "claude found at $(type -P claude)"

if have huginn; then
    ok "huginn found at $(type -P huginn) ($(huginn --version 2>&1 | head -1))"
    HUGINN_CMD="huginn"
elif [ -x "./bin/huginn" ]; then
    HUGINN_CMD="$(cd "$(dirname ./bin/huginn)" && pwd)/huginn"
    warn "huginn is not on your PATH; using $HUGINN_CMD"
    warn "run 'make install' to put it on your PATH and make this registration portable"
else
    printf '\n%s\n' "huginn is not installed."
    if [ -f go.mod ] && have go; then
        printf '  This looks like the source tree. Install it with:  %smake install%s\n' "$BOLD" "$N"
    else
        printf '  Build it with %smake install%s from the source tree, or copy a release binary onto your PATH.\n' "$BOLD" "$N"
    fi
    printf '  Run %s./pre-run.sh%s for a full dependency report.\n\n' "$BOLD" "$N"
    exit 1
fi

# Workspace root. Local tools refuse to start without a real directory.
if [ -z "$WORKSPACE" ]; then
    WORKSPACE="$PWD"
    warn "no --workspace given; using the current directory: $WORKSPACE"
fi
WORKSPACE_INPUT="$WORKSPACE"
WORKSPACE="$(cd "$WORKSPACE" 2>/dev/null && pwd)" || die "workspace directory does not exist: $WORKSPACE_INPUT"
[ -n "$WORKSPACE" ] || die "workspace directory does not exist: $WORKSPACE_INPUT"
[ -d "$WORKSPACE" ] || die "workspace is not a directory: $WORKSPACE"
ok "workspace root: $WORKSPACE"

# ---------------------------------------------------------------------------
# Verify the token before writing it anywhere
# ---------------------------------------------------------------------------

if [ -n "$TOKEN" ]; then
    step "Verifying the GitHub token"
    if have curl; then
        LIMIT="$(curl -sS -m 15 -H "Authorization: Bearer $TOKEN" \
                 https://api.github.com/rate_limit 2>/dev/null \
                 | grep -oE '"limit":[[:space:]]*[0-9]+' | head -1 | grep -oE '[0-9]+')"
        case "${LIMIT:-}" in
            ""|60)
                warn "GitHub is treating this token as anonymous (rate limit ${LIMIT:-unknown})."
                warn "It is probably expired, revoked, or mistyped."
                if [ "$FORCE" = 0 ]; then
                    printf '  Re-run with %s--force%s to register it anyway.\n' "$BOLD" "$N"
                    exit 1
                fi
                warn "--force given; registering the token regardless" ;;
            *)  ok "token accepted — rate limit ${LIMIT}/hour" ;;
        esac
    else
        warn "curl is not installed; skipping token verification"
    fi
fi

# ---------------------------------------------------------------------------
# Register
# ---------------------------------------------------------------------------

step "Registering '$NAME' with Claude Code (scope: $SCOPE)"

ENV_ARGS=(-e "ENABLE_LOCAL=true" -e "WORKSPACE_ROOT=$WORKSPACE" -e "METRICS_ENABLED=$METRICS_ENABLED")
[ "$METRICS_ENABLED" = "true" ] && ENV_ARGS+=(-e "METRICS_PORT=$METRICS_PORT")
[ -n "$TOKEN" ] && ENV_ARGS+=(-e "GITHUB_TOKEN=$TOKEN")

# Redacted echo of what will run, so the token never lands in a log or a
# screen recording.
REDACTED=()
for arg in "${ENV_ARGS[@]}"; do
    case "$arg" in GITHUB_TOKEN=*) REDACTED+=("GITHUB_TOKEN=***redacted***") ;; *) REDACTED+=("$arg") ;; esac
done
printf '  %s%s%s\n' "$DIM" "claude mcp add $NAME -s $SCOPE ${REDACTED[*]} -- $HUGINN_CMD" "$N"

if [ "$DRY_RUN" = 1 ]; then
    printf '\n%s--dry-run: nothing was changed.%s\n\n' "$BOLD" "$N"
    exit 0
fi

# Replace any existing registration; `claude mcp add` will not overwrite one.
if claude mcp list 2>/dev/null | grep -q "^${NAME}:"; then
    if [ "$FORCE" = 0 ]; then
        printf '\n  A server named %s'"'"'%s'"'"'%s is already registered.\n' "$BOLD" "$NAME" "$N"
        printf '  Replace it? [y/N] '
        read -r reply
        case "$reply" in [yY]*) ;; *) printf '  Left unchanged.\n\n'; exit 0 ;; esac
    fi
    claude mcp remove "$NAME" -s "$SCOPE" >/dev/null 2>&1 || claude mcp remove "$NAME" >/dev/null 2>&1 || true
    ok "removed the previous registration"
fi

if ! claude mcp add "$NAME" -s "$SCOPE" "${ENV_ARGS[@]}" -- "$HUGINN_CMD" 2>&1 | sed 's/^/  /'; then
    die "registration failed"
fi

# ---------------------------------------------------------------------------
# Verify it actually connects
# ---------------------------------------------------------------------------

step "Verifying the registration"

STATUS="$(claude mcp list 2>/dev/null | grep "^${NAME}:" || true)"
if [ -z "$STATUS" ]; then
    die "'$NAME' does not appear in 'claude mcp list' after registering"
fi

if printf '%s' "$STATUS" | grep -qi 'connected'; then
    ok "$STATUS"
else
    warn "$STATUS"
    warn "registered, but the health check did not report Connected. Run ./pre-run.sh to diagnose."
fi

printf '\n%s%s%s\n' "$BOLD" "────────────────────────────────────────────────────────────" "$N"
printf '%s✔ Huginn is registered.%s\n\n' "$G" "$N"
printf '  workspace  %s\n' "$WORKSPACE"
printf '  scope      %s\n' "$SCOPE"
printf '  github     %s\n' "$([ -n "$TOKEN" ] && echo 'configured — all 11 tools available' || echo 'not configured — 6 of 11 tools available')"
[ "$METRICS_ENABLED" = "true" ] && printf '  metrics    http://127.0.0.1:%s/metrics\n' "$METRICS_PORT"
printf '\n  %sRestart Claude Code%s for the tools to appear in a session.\n\n' "$BOLD" "$N"
