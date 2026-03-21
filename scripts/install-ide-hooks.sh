#!/bin/bash
# Install IDE hooks for akashi (Claude Code + Cursor).
#
# Copies the unified akashi-hook.sh to ~/.claude/hooks/ and registers it in
# ~/.claude/settings.json. Also reports Cursor configuration status.
#
# Generates an AKASHI_HOOKS_API_KEY on first run and writes it to:
#   ~/.akashi/hooks.key  (read by the hook script — no env var needed)
#   .env                 (read by the Docker server)
# This key is required when akashi runs in Docker because Docker's network
# translation means the server sees the gateway IP, not 127.0.0.1, so the
# localhostOnly guard would otherwise reject all hook requests with 403.
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
KEY_DIR="$HOME/.akashi"
KEY_FILE="$KEY_DIR/hooks.key"

echo "Installing akashi IDE hooks..."

# -- 0. Generate hooks API key ------------------------------------------------
#
# The server's /hooks/* endpoints are protected by localhostOnly. When akashi
# runs in Docker, the hook script calls localhost:8080 from the host but Docker
# translates it — the server sees the Docker bridge gateway IP, not 127.0.0.1.
# The localhostOnly middleware rejects that with 403. A shared key in
# ~/.akashi/hooks.key (hook script) + .env (server) solves this without
# requiring any manual env var configuration.

mkdir -p "$KEY_DIR"
chmod 700 "$KEY_DIR"

if [ -f "$KEY_FILE" ]; then
  HOOKS_KEY=$(cat "$KEY_FILE")
  echo "  [akashi] hooks key exists -> $KEY_FILE"
else
  if command -v openssl >/dev/null 2>&1; then
    HOOKS_KEY=$(openssl rand -hex 32)
  else
    HOOKS_KEY=$(python3 -c "import secrets; print(secrets.token_hex(32))")
  fi
  printf '%s' "$HOOKS_KEY" > "$KEY_FILE"
  chmod 600 "$KEY_FILE"
  echo "  [akashi] generated hooks key -> $KEY_FILE"
fi

# Write / update AKASHI_HOOKS_API_KEY in .env so the server picks it up on the
# next `docker compose up`. Skip if .env doesn't exist (e.g. CI / bare binary).
ENV_FILE="$PROJECT_DIR/.env"
if [ -f "$ENV_FILE" ]; then
  if grep -q "^AKASHI_HOOKS_API_KEY=" "$ENV_FILE"; then
    if [ "$(uname)" = "Darwin" ]; then
      sed -i '' "s|^AKASHI_HOOKS_API_KEY=.*|AKASHI_HOOKS_API_KEY=${HOOKS_KEY}|" "$ENV_FILE"
    else
      sed -i "s|^AKASHI_HOOKS_API_KEY=.*|AKASHI_HOOKS_API_KEY=${HOOKS_KEY}|" "$ENV_FILE"
    fi
    echo "  [akashi] updated .env -> AKASHI_HOOKS_API_KEY"
  else
    printf '\n# Added by make install-hooks — do not edit manually\nAKASHI_HOOKS_API_KEY=%s\n' "$HOOKS_KEY" >> "$ENV_FILE"
    echo "  [akashi] appended to .env -> AKASHI_HOOKS_API_KEY"
  fi
else
  echo "  [akashi] no .env found — skipping server-side key sync"
  echo "           Add the following line to your .env manually, then restart the server:"
  echo "           AKASHI_HOOKS_API_KEY=$(cat "$KEY_FILE")"
fi

# -- 1. Claude Code setup -----------------------------------------------------

mkdir -p "$HOOKS_DST"

# Remove orphaned hook scripts from previous installations.
for stale in akashi-trace-reminder.sh akashi-precheck-gate.sh akashi-check-marker.sh; do
    if [ -f "$HOOKS_DST/$stale" ]; then
        rm "$HOOKS_DST/$stale"
        echo "  [clean] removed $HOOKS_DST/$stale"
    fi
done

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

# Remove stale hook registrations that pointed at the old per-script hooks.
stale_scripts = {
    f"{hooks_dst}/akashi-trace-reminder.sh",
    f"{hooks_dst}/akashi-precheck-gate.sh",
    f"{hooks_dst}/akashi-check-marker.sh",
}
cleaned = 0
for event_type in list(hooks.keys()):
    original = hooks[event_type]
    filtered = []
    for entry in original:
        entry_hooks = [h for h in entry.get("hooks", []) if h.get("command") not in stale_scripts]
        if entry_hooks:
            entry["hooks"] = entry_hooks
            filtered.append(entry)
        else:
            cleaned += 1
    hooks[event_type] = filtered
if cleaned:
    print(f"  [claude] settings.json -> removed {cleaned} stale hook registration(s)")

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
if [ -f "$ENV_FILE" ]; then
  echo "If the akashi server is already running, restart it to pick up the new key:"
  echo "  docker compose restart akashi"
fi
