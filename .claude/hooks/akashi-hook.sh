#!/usr/bin/env bash
# akashi-hook.sh — Unified IDE hook for Claude Code and Cursor.
#
# Routes hook events to the akashi server's HTTP hook endpoints.
# Falls back to local behavior when the server is unreachable.
#
# Receives JSON on stdin from the IDE with fields like:
#   session_id, hook_event_name, tool_name, tool_input, cwd, etc.
#
# Returns JSON on stdout in the IDE's expected format.
set -euo pipefail

INPUT=$(cat)
AKASHI_URL="${AKASHI_URL:-http://localhost:8080}"

# Determine the hook event from the input JSON or the AKASHI_HOOK_EVENT env var.
# Claude Code sets hook_event_name in the JSON; Cursor uses similar conventions.
HOOK_EVENT="${AKASHI_HOOK_EVENT:-}"
if [ -z "$HOOK_EVENT" ]; then
  HOOK_EVENT=$(echo "$INPUT" | jq -r '.hook_event_name // empty' 2>/dev/null || true)
fi

# Map the event to an endpoint path.
case "$HOOK_EVENT" in
  SessionStart|sessionStart)
    ENDPOINT="/hooks/session-start"
    ;;
  PreToolUse|preToolUse|beforeShellExecution|beforeMCPExecution)
    ENDPOINT="/hooks/pre-tool-use"
    ;;
  PostToolUse|postToolUse|afterShellExecution|afterMCPExecution|afterFileEdit)
    ENDPOINT="/hooks/post-tool-use"
    ;;
  *)
    # Unknown event — pass through silently.
    echo '{"continue":true,"suppressOutput":true}'
    exit 0
    ;;
esac

# Try the HTTP endpoint first (fast path).
HOOK_HEADERS=""
if [ -n "${AKASHI_HOOKS_API_KEY:-}" ]; then
  HOOK_HEADERS="-H X-Akashi-Hook-Key:${AKASHI_HOOKS_API_KEY}"
fi

RESPONSE=$(echo "$INPUT" | curl -sf --max-time 3 \
  -H "Content-Type: application/json" \
  $HOOK_HEADERS \
  -d @- "${AKASHI_URL}${ENDPOINT}" 2>/dev/null) && {
  echo "$RESPONSE"
  exit 0
}

# --- Fallback: local behavior when server is unreachable ---

case "$HOOK_EVENT" in
  SessionStart|sessionStart)
    # Simple reminder (same as old session-start-check.sh).
    echo "[akashi] Server not reachable at ${AKASHI_URL}. Remember to call akashi_check before decisions." >&2
    exit 0
    ;;
  PreToolUse|preToolUse)
    TOOL_NAME=$(echo "$INPUT" | jq -r '.tool_name // empty' 2>/dev/null || true)
    case "$TOOL_NAME" in
      Edit|Write|MultiEdit)
        # Check the marker file (legacy fallback).
        MARKER="/tmp/akashi-checked-$(whoami)"
        if [ -f "$MARKER" ]; then
          # Check age: allow if less than 2 hours old.
          if [ "$(uname)" = "Darwin" ]; then
            MARKER_AGE=$(( $(date +%s) - $(stat -f %m "$MARKER") ))
          else
            MARKER_AGE=$(( $(date +%s) - $(stat -c %Y "$MARKER") ))
          fi
          if [ "$MARKER_AGE" -lt 7200 ]; then
            echo '{"continue":true,"suppressOutput":true}'
            exit 0
          fi
        fi
        echo "AKASHI GATE: Call akashi_check before making changes." >&2
        exit 2
        ;;
      *)
        echo '{"continue":true,"suppressOutput":true}'
        exit 0
        ;;
    esac
    ;;
  PostToolUse|postToolUse)
    TOOL_NAME=$(echo "$INPUT" | jq -r '.tool_name // empty' 2>/dev/null || true)
    case "$TOOL_NAME" in
      mcp__akashi__*)
        # Write marker file for the precheck gate fallback.
        touch "/tmp/akashi-checked-$(whoami)"
        echo '{"continue":true,"suppressOutput":true}'
        exit 0
        ;;
      Bash)
        COMMAND=$(echo "$INPUT" | jq -r '.tool_input.command // empty' 2>/dev/null || true)
        if echo "$COMMAND" | grep -q 'git commit'; then
          echo "[akashi] Consider calling akashi_trace to record this commit." >&2
        fi
        echo '{"continue":true,"suppressOutput":true}'
        exit 0
        ;;
      *)
        echo '{"continue":true,"suppressOutput":true}'
        exit 0
        ;;
    esac
    ;;
  *)
    echo '{"continue":true,"suppressOutput":true}'
    exit 0
    ;;
esac
