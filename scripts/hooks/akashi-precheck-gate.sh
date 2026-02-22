#!/bin/bash
# Claude Code PreToolUse hook.
# Fires before Edit, Write, or MultiEdit. Blocks the call if akashi_check
# has not been called recently, forcing Claude to establish decision context
# before touching any files.
#
# Installed by: make install-hooks
# Registered in: ~/.claude/settings.json

MARKER="/tmp/akashi-checked-$(whoami)"
MAX_AGE=7200  # 2 hours

if [ -f "$MARKER" ]; then
    # macOS uses stat -f %m; Linux uses stat -c %Y
    mtime=$(stat -f %m "$MARKER" 2>/dev/null || stat -c %Y "$MARKER" 2>/dev/null || echo 0)
    age=$(( $(date +%s) - mtime ))
    if [ "$age" -lt "$MAX_AGE" ]; then
        exit 0  # recent check on record — allow
    fi
fi

echo "AKASHI GATE: You have not called akashi_check in this session. Call it now before editing any files. Check for prior decisions and precedents, then proceed."
exit 2
