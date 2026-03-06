#!/bin/bash
# Install IDE hooks for akashi (Claude Code + Cursor).
#
# Copies the unified akashi-hook.sh to ~/.claude/hooks/ and registers it in
# ~/.claude/settings.json. Also reports Cursor configuration status.
#
# Safe to run multiple times (idempotent).
#
# Usage: make install-hooks
#   or:  bash scripts/install-ide-hooks.sh

set -euo pipefail

PROJECT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
HOOKS_SRC="$PROJECT_DIR/.claude/hooks"
HOOKS_DST="$HOME/.claude/hooks"
SETTINGS="$HOME/.claude/settings.json"

echo "Installing akashi IDE hooks..."

# -- 1. Claude Code setup -----------------------------------------------------

mkdir -p "$HOOKS_DST"

# Copy the unified hook script.
cp "$HOOKS_SRC/akashi-hook.sh" "$HOOKS_DST/akashi-hook.sh"
chmod +x "$HOOKS_DST/akashi-hook.sh"
echo "  [claude] hook script -> $HOOKS_DST/akashi-hook.sh"

# Register in ~/.claude/settings.json (idempotent).
python3 - <<'PYEOF'
import json, os, sys

settings_path = os.path.expanduser("~/.claude/settings.json")
hooks_dst = os.path.expanduser("~/.claude/hooks")

try:
    with open(settings_path) as f:
        settings = json.load(f)
except FileNotFoundError:
    settings = {}

hooks = settings.setdefault("hooks", {})
cmd = f"{hooks_dst}/akashi-hook.sh"

def has_command(hook_list, target_cmd):
    """Check if a hook list already contains our unified script."""
    for entry in hook_list:
        for h in entry.get("hooks", []):
            if h.get("command") == target_cmd:
                return True
    return False

registered = []

# SessionStart: context injection
session = hooks.setdefault("SessionStart", [])
if not has_command(session, cmd):
    session.append({"hooks": [{"type": "command", "command": cmd, "timeout": 5}]})
    registered.append("SessionStart")

# PreToolUse: edit gate + pre-commit reminder
pre = hooks.setdefault("PreToolUse", [])
if not has_command(pre, cmd):
    pre.append({"matcher": "Bash|Edit|Write|MultiEdit", "hooks": [{"type": "command", "command": cmd, "timeout": 10}]})
    registered.append("PreToolUse[Bash|Edit|Write|MultiEdit]")

# PostToolUse: check marker + auto-trace
post = hooks.setdefault("PostToolUse", [])
if not has_command(post, cmd):
    post.append({"matcher": "Bash|mcp__akashi__.*", "hooks": [{"type": "command", "command": cmd, "timeout": 15}]})
    registered.append("PostToolUse[Bash|mcp__akashi__.*]")

if registered:
    with open(settings_path, "w") as f:
        json.dump(settings, f, indent=2)
        f.write("\n")
    for r in registered:
        print(f"  [claude] settings.json -> registered {r}")
else:
    print("  [claude] settings.json -> all hooks already registered")
PYEOF

# -- 2. Cursor status ---------------------------------------------------------

if [ -d "$PROJECT_DIR/.cursor" ]; then
    echo "  [cursor] .cursor/ directory exists with:"
    [ -f "$PROJECT_DIR/.cursor/hooks.json" ] && echo "    - hooks.json (hook registration)"
    [ -f "$PROJECT_DIR/.cursor/mcp.json" ] && echo "    - mcp.json (MCP server config)"
    [ -f "$PROJECT_DIR/.cursor/rules/akashi.md" ] && echo "    - rules/akashi.md (workflow rule)"
    [ -L "$PROJECT_DIR/.cursor/hooks/akashi-hook.sh" ] && echo "    - hooks/akashi-hook.sh (symlink to unified script)"
    echo "  [cursor] No additional installation needed — Cursor reads .cursor/ from the project."
else
    echo "  [cursor] No .cursor/ directory found. Cursor integration not configured."
fi

echo ""
echo "Done. Hooks will be active in the next IDE session."
echo "Ensure the akashi server is running at \${AKASHI_URL:-http://localhost:8080} for full functionality."
