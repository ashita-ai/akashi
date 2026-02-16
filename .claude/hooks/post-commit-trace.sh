#!/bin/bash
# Akashi post-commit auto-trace hook for Claude Code.
#
# After a git commit, automatically creates an Akashi decision trace
# capturing what was committed and why. Runs as an async PostToolUse hook
# on the Bash tool — only fires after successful git commit commands.
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

# Extract commit info from the repo.
COMMIT_MSG=$(git log -1 --format='%s' 2>/dev/null || echo "")
if [ -z "$COMMIT_MSG" ]; then
  exit 0
fi

FILES_CHANGED=$(git diff-tree --no-commit-id --name-only -r HEAD 2>/dev/null | head -20 | tr '\n' ', ' | sed 's/,$//')
FILE_COUNT=$(git diff-tree --no-commit-id --name-only -r HEAD 2>/dev/null | wc -l | tr -d ' ')

# Build the trace payload. Uses the akashi MCP tools if available,
# otherwise falls back to a direct HTTP call.
REASONING="Committed ${FILE_COUNT} file(s): ${FILES_CHANGED}"

echo "[akashi] Auto-tracing commit: ${COMMIT_MSG}" >&2

# The hook signals success — the actual trace is created by the agent
# in the next turn via the akashi_trace MCP tool. We output a prompt
# suggestion that Claude Code will incorporate.
jq -n \
  --arg msg "$COMMIT_MSG" \
  --arg reasoning "$REASONING" \
  '{
    hookSpecificOutput: {
      hookEventName: "PostToolUse",
      message: ("akashi: auto-trace commit — call akashi_trace with decision_type=\"implementation\", outcome=\"" + $msg + "\", reasoning=\"" + $reasoning + "\", confidence=0.8")
    }
  }'
