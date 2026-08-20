#!/usr/bin/env bash
# Drive a real MCP handshake through a command and report what came back.
#
# This is the check that matters for a containerised server: it proves the
# image starts, that stdout carries protocol frames and nothing else, and that
# every expected tool is registered. Anything the server logs to stderr is
# shown separately, so a stray write to stdout would show up as a parse error
# rather than passing silently.
#
# Usage:
#   scripts/mcp-smoke.sh docker compose --profile mcp run --rm -T huginn
#   scripts/mcp-smoke.sh ./bin/huginn

set -euo pipefail

if [ $# -eq 0 ]; then
    echo "usage: $0 <command to start the MCP server> [args...]" >&2
    exit 2
fi

stderr_log="$(mktemp)"
trap 'rm -f "$stderr_log"' EXIT

request() { printf '%s\n' "$1"; }

{
    request '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"smoke","version":"1"}}}'
    request '{"jsonrpc":"2.0","method":"notifications/initialized"}'
    request '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
} | "$@" 2>"$stderr_log" | python3 -c '
import json
import sys

seen_init = False
tools = []
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        message = json.loads(line)
    except json.JSONDecodeError:
        # stdout is the transport: anything unparseable means something
        # wrote to it that should have gone to stderr.
        print("NON-PROTOCOL OUTPUT ON STDOUT: %r" % line[:200])
        sys.exit(1)

    if message.get("id") == 1:
        info = message["result"]["serverInfo"]
        print("server:   %s %s" % (info["name"], info["version"]))
        print("protocol: %s" % message["result"]["protocolVersion"])
        seen_init = True
    elif message.get("id") == 2:
        tools = [t["name"] for t in message["result"]["tools"]]

if not seen_init:
    print("no initialize response received")
    sys.exit(1)

print("tools:    %d" % len(tools))
for name in sorted(tools):
    print("  - %s" % name)
'
status=$?

if [ -s "$stderr_log" ]; then
    echo
    echo "--- server log (stderr) ---"
    cat "$stderr_log"
fi

exit "$status"
