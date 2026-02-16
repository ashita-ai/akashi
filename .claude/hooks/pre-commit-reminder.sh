#!/bin/bash
# Akashi pre-commit precedent check reminder for Claude Code.
#
# Before a git commit, emits an advisory message if the agent hasn't
# called akashi_check during the session. Non-blocking — the commit
# still proceeds, but the agent sees the reminder.
#
# Install: copy .claude/settings.json.example to .claude/settings.json
# (or merge the hooks section into your existing settings).

set -euo pipefail

INPUT=$(cat)

# Only act on Bash tool calls that look like git commits.
COMMAND=$(echo "$INPUT" | jq -r '.tool_input.command // ""')
if ! echo "$COMMAND" | grep -q 'git commit'; then
  exit 0
fi

# Check if the transcript contains an akashi_check call.
TRANSCRIPT=$(echo "$INPUT" | jq -r '.transcript_path // ""')
if [ -n "$TRANSCRIPT" ] && [ -f "$TRANSCRIPT" ]; then
  if grep -q 'akashi_check' "$TRANSCRIPT" 2>/dev/null; then
    exit 0  # Already checked — no reminder needed.
  fi
fi

# Advisory only — non-blocking (exit 0, message via stderr).
echo "[akashi] No akashi_check call found this session. Consider checking for precedents before committing." >&2
exit 0
