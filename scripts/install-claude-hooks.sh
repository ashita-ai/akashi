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
HOOKS_SRC="$SCRIPT_DIR/hooks"
HOOKS_DST="$HOME/.claude/hooks"
SETTINGS="$HOME/.claude/settings.json"

mkdir -p "$HOOKS_DST"

# -- 1. Install hook scripts --------------------------------------------------
for script in akashi-trace-reminder.sh akashi-precheck-gate.sh akashi-check-marker.sh; do
    cp "$HOOKS_SRC/$script" "$HOOKS_DST/$script"
    chmod +x "$HOOKS_DST/$script"
    echo "  hook script -> $HOOKS_DST/$script"
done

# -- 2. Register in ~/.claude/settings.json (idempotent) ---------------------
python3 - <<EOF
import json, os, sys

settings_path = "$SETTINGS"
hooks_dst = "$HOOKS_DST"

try:
    with open(settings_path) as f:
        settings = json.load(f)
except FileNotFoundError:
    settings = {}

hooks = settings.setdefault("hooks", {})

def already_registered(hook_list, matcher, cmd):
    for entry in hook_list:
        if entry.get("matcher") == matcher:
            for h in entry.get("hooks", []):
                if h.get("command") == cmd:
                    return True
    return False

def add_hook(hook_list, matcher, cmd):
    if not already_registered(hook_list, matcher, cmd):
        hook_list.append({"matcher": matcher, "hooks": [{"type": "command", "command": cmd}]})
        return True
    return False

registered = []

# PostToolUse: trace reminder after git commit
post = hooks.setdefault("PostToolUse", [])
if add_hook(post, "Bash", f"{hooks_dst}/akashi-trace-reminder.sh"):
    registered.append("PostToolUse[Bash] -> akashi-trace-reminder.sh")

# PostToolUse: write marker after akashi_check or akashi_trace
for mcp_tool in ("mcp__akashi__akashi_check", "mcp__akashi__akashi_trace"):
    if add_hook(post, mcp_tool, f"{hooks_dst}/akashi-check-marker.sh"):
        registered.append(f"PostToolUse[{mcp_tool}] -> akashi-check-marker.sh")

# PreToolUse: gate on Edit/Write/MultiEdit until akashi_check is on record
pre = hooks.setdefault("PreToolUse", [])
for tool in ("Edit", "Write", "MultiEdit"):
    if add_hook(pre, tool, f"{hooks_dst}/akashi-precheck-gate.sh"):
        registered.append(f"PreToolUse[{tool}] -> akashi-precheck-gate.sh")

if registered:
    with open(settings_path, "w") as f:
        json.dump(settings, f, indent=2)
        f.write("\n")
    for r in registered:
        print(f"  settings.json -> registered {r}")
else:
    print("  settings.json -> all hooks already registered, skipping")
EOF

echo "Done. Hooks active for the next Claude Code session."
