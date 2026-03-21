#!/usr/bin/env bash
# akashi-hook.sh — Unified IDE hook for Claude Code and Cursor.
#
# Routes hook events to the akashi server's HTTP hook endpoints.
# Falls back to local behavior only when the server is genuinely unreachable.
#
# Receives JSON on stdin from the IDE with fields like:
#   session_id, hook_event_name, tool_name, tool_input, cwd, etc.
#
# Returns JSON on stdout in the IDE's expected format.
#
# API key loading (in priority order):
#   1. AKASHI_HOOKS_API_KEY env var (if set)
#   2. ~/.akashi/hooks.key (written by `make install-hooks` — no manual config needed)
# The key is required when akashi runs in Docker because Docker's network
# translation means the server sees the gateway IP, not 127.0.0.1.
set -euo pipefail

INPUT=$(cat)
AKASHI_URL="${AKASHI_URL:-http://localhost:8080}"

# Load the hooks API key. Prefer env var; fall back to the key file written by
# `make install-hooks`. This means no shell env var configuration is needed.
if [ -z "${AKASHI_HOOKS_API_KEY:-}" ] && [ -f "$HOME/.akashi/hooks.key" ]; then
  AKASHI_HOOKS_API_KEY=$(cat "$HOME/.akashi/hooks.key")
fi

# Extract agent_id from AKASHI_API_KEY (format: "agent_id:secret").
# This lets the server gate checks per-agent rather than per-machine.
AGENT_ID=""
if [ -n "${AKASHI_API_KEY:-}" ]; then
  AGENT_ID="${AKASHI_API_KEY%%:*}"
fi

# Inject agent_id into the hook JSON payload if we have one.
if [ -n "$AGENT_ID" ]; then
  INPUT=$(echo "$INPUT" | jq --arg aid "$AGENT_ID" '. + {agent_id: $aid}' 2>/dev/null || echo "$INPUT")
fi

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

# Build auth args array (safe quoting even if key contains special characters).
AUTH_ARGS=()
if [ -n "${AKASHI_HOOKS_API_KEY:-}" ]; then
  AUTH_ARGS=(-H "X-Akashi-Hook-Key: ${AKASHI_HOOKS_API_KEY}")
fi

# Call the server. Capture both the body and the HTTP status code so we can
# distinguish "server down" (curl fails to connect) from "auth error" (HTTP 403).
# We append a sentinel to the output and strip it after — this avoids a second
# curl call just to get the status code.
RAW=$(echo "$INPUT" | curl -s \
  -o /dev/stdout -w $'\n__AKASHI_STATUS__%{http_code}' \
  --max-time 3 \
  -H "Content-Type: application/json" \
  "${AUTH_ARGS[@]}" \
  -d @- "${AKASHI_URL}${ENDPOINT}" 2>/dev/null) || true

HTTP_STATUS=$(printf '%s' "$RAW" | grep -oE '__AKASHI_STATUS__[0-9]+$' | grep -oE '[0-9]+$' || true)
HTTP_BODY=$(printf '%s' "$RAW" | sed 's/__AKASHI_STATUS__[0-9]*$//')

case "$HTTP_STATUS" in
  2[0-9][0-9])
    # Success — return the server's response directly.
    printf '%s' "$HTTP_BODY"
    exit 0
    ;;
  403)
    # Server is running but rejected the key. The key in ~/.akashi/hooks.key
    # doesn't match AKASHI_HOOKS_API_KEY in the server's .env. Surface a clear
    # error rather than silently falling through to the permissive fallback.
    echo '[akashi] Hook authentication failed (HTTP 403). The hooks API key is out of sync.' >&2
    echo '[akashi] Fix: run `make install-hooks`, then `docker compose restart akashi`.' >&2
    TOOL_NAME=$(echo "$INPUT" | jq -r '.tool_name // empty' 2>/dev/null || true)
    if [ "$TOOL_NAME" = "Edit" ] || [ "$TOOL_NAME" = "Write" ] || [ "$TOOL_NAME" = "MultiEdit" ]; then
      echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"akashi hook auth failed — run `make install-hooks` then `docker compose restart akashi`"}}'
    else
      echo '{"continue":true,"suppressOutput":true}'
    fi
    exit 0
    ;;
  '')
    # curl returned nothing — server is unreachable (not started, wrong URL, etc.).
    # Fall through to local fallback below.
    ;;
  *)
    # Unexpected HTTP status — fall through to local fallback.
    ;;
esac

# --- Fallback: local behavior when server is genuinely unreachable -----------
# This path only runs when curl cannot connect at all. It does NOT run on auth
# errors — those are caught above with a hard block on edit tools.

case "$HOOK_EVENT" in
  SessionStart|sessionStart)
    echo "[akashi] Server not reachable at ${AKASHI_URL}. Start it with: docker compose up -d" >&2
    exit 0
    ;;
  PreToolUse|preToolUse)
    TOOL_NAME=$(echo "$INPUT" | jq -r '.tool_name // empty' 2>/dev/null || true)
    case "$TOOL_NAME" in
      Edit|Write|MultiEdit)
        # Check an agent-scoped marker written by the PostToolUse fallback path.
        # Scoping by agent_id prevents one agent's check from unlocking edits
        # for a different agent on the same machine.
        FALLBACK_AGENT=$(echo "$INPUT" | jq -r '.agent_id // empty' 2>/dev/null || true)
        if [ -n "$FALLBACK_AGENT" ]; then
          MARKER="/tmp/akashi-checked-${FALLBACK_AGENT}"
        else
          MARKER="/tmp/akashi-checked-$(whoami)"
        fi
        if [ -f "$MARKER" ]; then
          if [ "$(uname)" = "Darwin" ]; then
            MARKER_AGE=$(( $(date +%s) - $(stat -f %m "$MARKER") ))
          else
            MARKER_AGE=$(( $(date +%s) - $(stat -c %Y "$MARKER") ))
          fi
          if [ "$MARKER_AGE" -lt 3600 ]; then
            echo '{"continue":true,"suppressOutput":true}'
            exit 0
          fi
        fi
        echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"Call akashi_check before making changes (akashi server offline — using local fallback)."}}'
        exit 0
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
        # Write an agent-scoped marker for the precheck fallback gate.
        FALLBACK_AGENT=$(echo "$INPUT" | jq -r '.agent_id // empty' 2>/dev/null || true)
        if [ -n "$FALLBACK_AGENT" ]; then
          touch "/tmp/akashi-checked-${FALLBACK_AGENT}"
        else
          touch "/tmp/akashi-checked-$(whoami)"
        fi
        echo '{"continue":true,"suppressOutput":true}'
        exit 0
        ;;
      Bash)
        COMMAND=$(echo "$INPUT" | jq -r '.tool_input.command // empty' 2>/dev/null || true)
        if echo "$COMMAND" | grep -q 'git commit'; then
          echo "[akashi] Server offline. Call akashi_trace manually to record this commit." >&2
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
