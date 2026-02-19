#!/bin/bash
# Claude Code PostToolUse hook.
# Fires after every Bash tool call. If it was a git commit, reminds Claude to
# record the decision in akashi before moving on.
#
# Installed by: make install-hooks
# Registered in: ~/.claude/settings.json

command=$(echo "$CLAUDE_TOOL_INPUT" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    print(d.get('command', ''))
except Exception:
    print('')
" 2>/dev/null)

if echo "$command" | grep -qE 'git commit'; then
    echo "AKASHI: git commit detected. Call akashi_trace before moving on — record what you decided and why."
fi
