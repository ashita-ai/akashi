#!/bin/bash
# Akashi session-start hook for Claude Code.
#
# On session start, reminds the agent to check recent Akashi decisions
# for context. This is the automation equivalent of the CLAUDE.md instruction
# "call akashi_recent at session start."
#
# Install: copy .claude/settings.json.example to .claude/settings.json
# (or merge the hooks section into your existing settings).

set -euo pipefail

echo "[akashi] Session started. Remember to call akashi_recent for context." >&2

exit 0
