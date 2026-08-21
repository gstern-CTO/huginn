#!/usr/bin/env bash
#
# pre-run.sh — check everything Huginn can use, and report all of it at once.
#
# This never stops at the first problem. It runs every check, then prints one
# report, because finding out about five missing things one run at a time is
# the slowest possible way to set up a machine.
#
# Exit status:
#   0  every REQUIRED dependency is present
#   1  at least one REQUIRED dependency is missing
#
# Recommended and optional items never affect the exit status: Huginn runs
# without them and degrades tool by tool, which the report spells out.
#
# Usage:
#   ./pre-run.sh              # check everything
#   ./pre-run.sh --quiet      # only show problems
#   ./pre-run.sh --no-color   # plain output, for logs and CI

set -uo pipefail

QUIET=0
COLOR=1
for arg in "$@"; do
    case "$arg" in
        --quiet|-q)    QUIET=1 ;;
        --no-color)    COLOR=0 ;;
        --help|-h)
            sed -n '3,20p' "$0" | sed 's/^# \{0,1\}//'
            exit 0 ;;
        *) echo "unknown option: $arg (try --help)" >&2; exit 2 ;;
    esac
done

if [ "$COLOR" = 1 ] && [ -t 1 ]; then
    R=$'\033[31m'; G=$'\033[32m'; Y=$'\033[33m'; B=$'\033[34m'; DIM=$'\033[2m'; BOLD=$'\033[1m'; N=$'\033[0m'
else
    R=""; G=""; Y=""; B=""; DIM=""; BOLD=""; N=""
fi

# Field separator for the missing-item records. ASCII unit separator, because
# install strings legitimately contain "|" to separate per-platform commands.
SEP=$'\x1f'

MISSING_REQUIRED=()
MISSING_RECOMMENDED=()
MISSING_OPTIONAL=()
NOTES=()

ok()      { [ "$QUIET" = 1 ] || printf '  %s✔%s %-32s %s%s%s\n' "$G" "$N" "$1" "$DIM" "${2:-}" "$N"; }
bad()     { printf '  %s✘%s %-32s %s%s%s\n' "$R" "$N" "$1" "$DIM" "${2:-}" "$N"; }
warn()    { printf '  %s!%s %-32s %s%s%s\n' "$Y" "$N" "$1" "$DIM" "${2:-}" "$N"; }
section() { [ "$QUIET" = 1 ] || printf '\n%s%s%s\n' "$BOLD" "$1" "$N"; }

# have <binary> — true only for a real executable on PATH. `command -v` is not
# used here because it also matches shell functions and aliases, which cannot
# be exec'd by Huginn. A shell wrapper named `rg` would otherwise report
# ripgrep as present when no ripgrep binary exists.
have() { type -P "$1" >/dev/null 2>&1; }

version_of() {
    case "$1" in
        go)     go version 2>/dev/null | awk '{print $3}' ;;
        rg)     rg --version 2>/dev/null | head -1 | awk '{print $2}' ;;
        git)    git --version 2>/dev/null | awk '{print $3}' ;;
        docker) docker --version 2>/dev/null | awk '{print $3}' | tr -d ',' ;;
        python3) python3 --version 2>/dev/null | awk '{print $2}' ;;
        claude) claude --version 2>/dev/null | head -1 ;;
        *)      "$1" --version 2>/dev/null | head -1 ;;
    esac
}

# check <level> <binary> <what it is for> <how to install>
check() {
    local level="$1" bin="$2" purpose="$3" install="$4" path ver
    if have "$bin"; then
        path="$(type -P "$bin")"; ver="$(version_of "$bin")"
        ok "$bin" "${ver:-$path}"
        return 0
    fi
    case "$level" in
        required)    bad  "$bin" "$purpose"; MISSING_REQUIRED+=("$bin$SEP$purpose$SEP$install") ;;
        recommended) warn "$bin" "$purpose"; MISSING_RECOMMENDED+=("$bin$SEP$purpose$SEP$install") ;;
        optional)    [ "$QUIET" = 1 ] || printf '  %s-%s %-32s %s%s%s\n' "$DIM" "$N" "$bin" "$DIM" "$purpose" "$N"
                     MISSING_OPTIONAL+=("$bin$SEP$purpose$SEP$install") ;;
    esac
    return 1
}

[ "$QUIET" = 1 ] || printf '%sHuginn dependency check%s  %s%s %s/%s%s\n' \
    "$BOLD" "$N" "$DIM" "$(date +%Y-%m-%d\ %H:%M)" "$(uname -s)" "$(uname -m)" "$N"

# ---------------------------------------------------------------------------
section "Core — Huginn cannot start without these"
# ---------------------------------------------------------------------------

# The binary itself, or a way to build it. Either satisfies the requirement.
HUGINN_PRESENT=0
if have huginn; then
    ok "huginn" "$(huginn --version 2>&1 | head -1) at $(type -P huginn)"
    HUGINN_PRESENT=1
elif [ -x "./bin/huginn" ]; then
    ok "huginn" "./bin/huginn (not on PATH — run 'make install')"
    HUGINN_PRESENT=1
else
    bad "huginn" "the server binary itself"
    MISSING_REQUIRED+=("huginn${SEP}the server binary${SEP}build it with 'make install', or copy a release binary onto your PATH")
fi

# Go is required only to build from source. If the binary is already present it
# is merely recommended, which is why this is not a plain `check`.
GO_MIN="1.25.5"
if have go; then
    GO_VER="$(go version | awk '{print $3}' | sed 's/^go//')"
    if [ "$(printf '%s\n%s\n' "$GO_MIN" "$GO_VER" | sort -V | head -1)" = "$GO_MIN" ]; then
        ok "go" "$GO_VER (>= $GO_MIN)"
    else
        warn "go" "$GO_VER is older than $GO_MIN required by go.mod"
        NOTES+=("Go $GO_VER predates the $GO_MIN in go.mod. With the default GOTOOLCHAIN=auto the right toolchain downloads automatically, so this usually resolves itself.")
    fi
elif [ "$HUGINN_PRESENT" = 0 ]; then
    bad "go" "needed to build huginn from source"
    MISSING_REQUIRED+=("go${SEP}to build huginn (>= $GO_MIN)${SEP}https://go.dev/dl/ — or install a prebuilt huginn binary instead")
else
    warn "go" "absent, but the binary already exists so nothing to build"
fi

check recommended git "cloning, and the git tool on the exec allowlist" \
      "apt install git | brew install git"

# ---------------------------------------------------------------------------
section "MCP client — needed to register and actually use Huginn"
# ---------------------------------------------------------------------------

if have claude; then
    ok "claude" "$(type -P claude)"
    if claude mcp list 2>/dev/null | grep -q '^huginn:'; then
        ok "huginn registered" "$(claude mcp list 2>/dev/null | grep '^huginn:' | sed 's/^huginn: *//')"
    else
        warn "huginn registered" "not registered yet — run ./huginn.sh --git-token ghp_..."
        NOTES+=("Huginn is installed but not registered with any MCP client. Run ./huginn.sh to register it.")
    fi
else
    warn "claude" "Claude Code CLI, to register Huginn as an MCP server"
    MISSING_RECOMMENDED+=("claude${SEP}to register Huginn as an MCP server${SEP}https://claude.com/claude-code — or register manually in Cursor or another MCP client")
fi

# ---------------------------------------------------------------------------
section "Tool dependencies — each missing item disables one tool"
# ---------------------------------------------------------------------------

check recommended rg "local_search_code — without it that tool reports DEPENDENCY_MISSING" \
      "apt install ripgrep | brew install ripgrep | cargo install ripgrep"

check optional find "on the exec allowlist; local_find_files uses Go's own walker so this is not needed" \
      "part of coreutils/findutils on every platform"

check optional gh "last-resort GitHub token source via 'gh auth token'" \
      "apt install gh | brew install gh"

# ---------------------------------------------------------------------------
section "Language servers — lsp_navigate falls back to ripgrep without one"
# ---------------------------------------------------------------------------

LSP_FOUND=0
lsp_check() {
    if have "$1"; then ok "$1" "$2 files"; LSP_FOUND=$((LSP_FOUND + 1))
    else [ "$QUIET" = 1 ] || printf '  %s-%s %-32s %s%s%s\n' "$DIM" "$N" "$1" "$DIM" "$2 files" "$N"
         MISSING_OPTIONAL+=("$1${SEP}lsp_navigate for $2$SEP$3")
    fi
}
lsp_check gopls                      ".go"          "go install golang.org/x/tools/gopls@latest"
lsp_check pyright-langserver         ".py"          "npm install -g pyright"
lsp_check rust-analyzer              ".rs"          "rustup component add rust-analyzer"
lsp_check typescript-language-server ".ts/.js"      "npm install -g typescript-language-server typescript"
lsp_check clangd                     ".c/.cpp"      "apt install clangd | brew install llvm"
lsp_check solargraph                 ".rb"          "gem install solargraph"
lsp_check lua-language-server        ".lua"         "brew install lua-language-server"
lsp_check jdtls                      ".java"        "see the Eclipse JDT language server docs"
lsp_check intelephense               ".php"         "npm install -g intelephense"

if [ "$LSP_FOUND" = 0 ]; then
    NOTES+=("No language server found. lsp_navigate still works — it falls back to a ripgrep symbol search and names the install command — but results are textual and may contain false positives.")
fi

# ---------------------------------------------------------------------------
section "Configuration"
# ---------------------------------------------------------------------------

# Token, in the order Huginn resolves it.
TOKEN_SOURCE=""
for var in OCTOCODE_TOKEN GH_TOKEN GITHUB_TOKEN; do
    if [ -n "${!var:-}" ]; then TOKEN_SOURCE="$var"; break; fi
done
if [ -z "$TOKEN_SOURCE" ] && have gh && gh auth token >/dev/null 2>&1; then
    TOKEN_SOURCE="gh auth token"
fi

if [ -n "$TOKEN_SOURCE" ]; then
    ok "GitHub token" "found via $TOKEN_SOURCE"
    if have curl; then
        LIMIT="$(curl -sS -m 10 -H "Authorization: Bearer ${!TOKEN_SOURCE:-$(gh auth token 2>/dev/null)}" \
                 https://api.github.com/rate_limit 2>/dev/null \
                 | grep -oE '"limit":[[:space:]]*[0-9]+' | head -1 | grep -oE '[0-9]+')"
        case "${LIMIT:-}" in
            5000|15000) ok "token accepted" "rate limit $LIMIT/hour" ;;
            60)  warn "token rejected" "rate limit 60/hour — the token is not being accepted"
                 NOTES+=("A GitHub token was found but GitHub is treating requests as anonymous. It is probably expired or revoked.") ;;
            "")  warn "token unverified" "could not reach api.github.com" ;;
            *)   ok "token accepted" "rate limit $LIMIT/hour" ;;
        esac
    fi
else
    warn "GitHub token" "not set — the 5 GitHub tools will report NOT_CONFIGURED"
    MISSING_RECOMMENDED+=("GITHUB_TOKEN${SEP}the 5 GitHub tools${SEP}create one at https://github.com/settings/tokens — no scopes needed for public repositories")
fi

# Local tools need a workspace root that exists.
if [ "${ENABLE_LOCAL:-false}" = "true" ]; then
    if [ -z "${WORKSPACE_ROOT:-}" ]; then
        bad "WORKSPACE_ROOT" "ENABLE_LOCAL=true but no workspace root set — Huginn will refuse to start"
        MISSING_REQUIRED+=("WORKSPACE_ROOT${SEP}required whenever ENABLE_LOCAL=true${SEP}export WORKSPACE_ROOT=/path/to/your/code")
    elif [ ! -d "${WORKSPACE_ROOT}" ]; then
        bad "WORKSPACE_ROOT" "${WORKSPACE_ROOT} does not exist — Huginn fails fast at startup"
        MISSING_REQUIRED+=("WORKSPACE_ROOT${SEP}points at a directory that does not exist${SEP}export WORKSPACE_ROOT=/a/directory/that/exists")
    else
        ok "WORKSPACE_ROOT" "${WORKSPACE_ROOT}"
    fi
    ok "ENABLE_LOCAL" "true — the 5 local and LSP tools are enabled"
else
    warn "ENABLE_LOCAL" "not true — the 4 local tools and lsp_navigate will not be registered"
    NOTES+=("Local tools are off. Set ENABLE_LOCAL=true and WORKSPACE_ROOT=/path/to/code to enable the filesystem and LSP tools.")
fi

# Databricks: all three variables of an environment, or none.
for env_name in DEV PROD; do
    host="DATABRICKS_${env_name}_HOST"; tok="DATABRICKS_${env_name}_TOKEN"; wh="DATABRICKS_${env_name}_WAREHOUSE_ID"
    if [ -n "${!host:-}" ] || [ -n "${!tok:-}" ] || [ -n "${!wh:-}" ]; then
        if [ -n "${!host:-}" ] && [ -n "${!tok:-}" ] && [ -n "${!wh:-}" ]; then
            ok "Databricks ${env_name,,}" "configured"
        else
            warn "Databricks ${env_name,,}" "partially configured — needs HOST, TOKEN and WAREHOUSE_ID"
            NOTES+=("Databricks ${env_name,,} is half-configured. All three of $host, $tok and $wh are required.")
        fi
    else
        [ "$QUIET" = 1 ] || printf '  %s-%s %-32s %s%s%s\n' "$DIM" "$N" "Databricks ${env_name,,}" "$DIM" "not configured — databricks_query unavailable for this environment" "$N"
    fi
done

# Cache directory must be writable, or the disk tier silently degrades.
CACHE="${CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/go-research-mcp}"
if mkdir -p "$CACHE" 2>/dev/null && [ -w "$CACHE" ]; then
    ok "cache directory" "$CACHE"
else
    warn "cache directory" "$CACHE is not writable — the disk cache tier will be skipped"
    NOTES+=("The cache directory is not writable, so Huginn runs memory-only and every session starts cold.")
fi

# Metrics port, only when metrics are on (they default to on).
if [ "${METRICS_ENABLED:-true}" != "false" ]; then
    PORT="${METRICS_PORT:-9090}"
    if have ss && ss -ltn 2>/dev/null | grep -q ":${PORT} "; then
        warn "metrics port ${PORT}" "already in use — Huginn logs a warning and carries on"
        NOTES+=("Port ${PORT} is occupied. Metrics will be unavailable, but the MCP server is unaffected. Set METRICS_PORT to change it.")
    else
        ok "metrics port ${PORT}" "free"
    fi
fi

# ---------------------------------------------------------------------------
section "Extras"
# ---------------------------------------------------------------------------

check optional docker  "running Huginn in a container instead of natively" \
      "https://docs.docker.com/get-docker/"
check optional python3 "scripts/mcp-smoke.sh, the protocol handshake test" \
      "apt install python3 | brew install python3"
check optional curl    "verifying the GitHub token in this script" \
      "apt install curl | brew install curl"

# ---------------------------------------------------------------------------
# Report
# ---------------------------------------------------------------------------

printf '\n%s%s%s\n' "$BOLD" "────────────────────────────────────────────────────────────" "$N"

print_group() {
    local title="$1" colour="$2"; shift 2
    local items=("$@")
    [ "${#items[@]}" -eq 0 ] && return 0
    printf '\n%s%s (%d)%s\n' "$colour" "$title" "${#items[@]}" "$N"
    for item in "${items[@]}"; do
        printf '  %s%-28s%s %s\n' "$BOLD" "${item%%"$SEP"*}" "$N" "$(printf '%s' "$item" | cut -d"$SEP" -f2)"
        printf '  %s%-28s  install: %s%s\n' "$DIM" "" "$(printf '%s' "$item" | cut -d"$SEP" -f3)" "$N"
    done
}

print_group "MISSING — REQUIRED"     "$R" ${MISSING_REQUIRED+"${MISSING_REQUIRED[@]}"}
print_group "MISSING — RECOMMENDED"  "$Y" ${MISSING_RECOMMENDED+"${MISSING_RECOMMENDED[@]}"}
print_group "MISSING — OPTIONAL"     "$B" ${MISSING_OPTIONAL+"${MISSING_OPTIONAL[@]}"}

if [ "${#NOTES[@]}" -gt 0 ]; then
    printf '\n%sNotes%s\n' "$BOLD" "$N"
    for note in "${NOTES[@]}"; do printf '  %s•%s %s\n' "$DIM" "$N" "$note"; done
fi

printf '\n'
if [ "${#MISSING_REQUIRED[@]}" -gt 0 ]; then
    printf '%s✘ %d required dependency/dependencies missing — Huginn will not run.%s\n' "$R" "${#MISSING_REQUIRED[@]}" "$N"
    printf '  Install the items under MISSING — REQUIRED above, then run this script again.\n\n'
    exit 1
fi

printf '%s✔ All required dependencies present.%s\n' "$G" "$N"
if [ "${#MISSING_RECOMMENDED[@]}" -gt 0 ]; then
    printf '  %d recommended item(s) missing — Huginn runs, with those tools degraded.\n' "${#MISSING_RECOMMENDED[@]}"
fi
printf '  Next: %s./huginn.sh --git-token ghp_...%s to register Huginn with Claude Code.\n\n' "$BOLD" "$N"
exit 0
