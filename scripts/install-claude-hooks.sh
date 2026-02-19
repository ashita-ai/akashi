#!/bin/bash
# Install Claude Code hooks for the akashi workspace.
#
# Copies hook scripts to ~/.claude/hooks/ and registers them in
# ~/.claude/settings.json. Safe to run multiple times (idempotent).
#
# Usage: make install-hooks
#   or:  bash scripts/install-claude-hooks.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HOOK_SRC="$SCRIPT_DIR/hooks/akashi-trace-reminder.sh"
HOOK_DST="$HOME/.claude/hooks/akashi-trace-reminder.sh"
SETTINGS="$HOME/.claude/settings.json"

# -- 1. Install hook script ---------------------------------------------------
mkdir -p "$HOME/.claude/hooks"
cp "$HOOK_SRC" "$HOOK_DST"
chmod +x "$HOOK_DST"
echo "  hook script -> $HOOK_DST"

# -- 2. Register in ~/.claude/settings.json (idempotent) ---------------------
python3 - <<EOF
import json, os, sys

settings_path = "$SETTINGS"
hook_cmd = "$HOOK_DST"

try:
    with open(settings_path) as f:
        settings = json.load(f)
except FileNotFoundError:
    settings = {}

hooks = settings.setdefault("hooks", {})
post = hooks.setdefault("PostToolUse", [])

# Already registered?
for entry in post:
    for h in entry.get("hooks", []):
        if h.get("command") == hook_cmd:
            print(f"  settings.json -> already registered, skipping")
            sys.exit(0)

post.append({
    "matcher": "Bash",
    "hooks": [{"type": "command", "command": hook_cmd}]
})

with open(settings_path, "w") as f:
    json.dump(settings, f, indent=2)
    f.write("\n")

print(f"  settings.json -> registered PostToolUse hook")
EOF

echo "Done. Hooks active for the next Claude Code session."
