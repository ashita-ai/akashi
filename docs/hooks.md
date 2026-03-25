# IDE Hook Endpoints

Akashi provides HTTP hook endpoints that integrate with Claude Code and Cursor to enforce
decision hygiene during development. Hooks inject recent decision context on session start,
gate file edits until `akashi_check` is called, and optionally auto-trace git commits.

## Quick setup

```bash
# Install hooks into Claude Code (idempotent, safe to re-run)
./scripts/install-ide-hooks.sh
```

This copies the unified hook script to `~/.claude/hooks/akashi-hook.sh` and registers
three hook events in `~/.claude/settings.json`.

## How it works

### Session start

When an IDE session begins, Akashi returns the 5 most recent decisions and any open
conflicts for the current project (inferred from git remote or directory name). This
context appears in the IDE sidebar so the agent starts with awareness of prior work.

### Edit gate

Before any `Edit`, `Write`, or `MultiEdit` tool call, the hook checks whether
`akashi_check` has been called in the current session (within the last 10 minutes).
If not, the edit is **blocked** with a message asking the agent to call `akashi_check`
first. Once checked, edits proceed normally until the 10-minute TTL expires.

### Commit tracing

After a `git commit`, the hook either:

- **Auto-traces** the commit as a decision (`decision_type: "implementation"`,
  `confidence: 0.5`) when `AKASHI_AUTO_TRACE=true` (the default), or
- **Suggests** running `akashi_trace` manually when auto-trace is disabled.

## Endpoints

All endpoints are `POST`, unauthenticated, and localhost-restricted by default.

| Endpoint | IDE Event | Purpose |
|----------|-----------|---------|
| `/hooks/session-start` | `SessionStart` | Inject recent decisions and open conflicts |
| `/hooks/pre-tool-use` | `PreToolUse` | Gate edits until `akashi_check` is called |
| `/hooks/post-tool-use` | `PostToolUse` | Record check markers; auto-trace commits |

### Request format

All endpoints accept the same JSON envelope from the IDE:

```json
{
  "session_id": "unique-session-uuid",
  "tool_name": "Edit",
  "tool_input": { "command": "git commit -m 'fix: typo'" },
  "hook_event_name": "PreToolUse",
  "cwd": "/path/to/workspace"
}
```

### Response format

```json
{
  "continue": true,
  "suppressOutput": false,
  "hookSpecificOutput": {
    "hookEventName": "PreToolUse",
    "additionalContext": "...",
    "permissionDecision": "deny",
    "permissionDecisionReason": "Call akashi_check before making changes.",
    "message": "[akashi] auto-traced commit: fix: typo"
  }
}
```

Key fields:

- **`continue`**: `false` blocks the tool call (used by the edit gate).
- **`permissionDecision`**: `"deny"` tells the IDE to reject the tool call.
- **`additionalContext`**: Rich text injected into the IDE context window.
- **`message`**: User-visible suggestion or confirmation.

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `AKASHI_HOOKS_ENABLED` | `true` | Register `/hooks/*` routes. Set `false` to disable entirely. |
| `AKASHI_HOOKS_API_KEY` | _(empty)_ | Shared secret for non-localhost access. Clients send `X-Akashi-Hook-Key` header. |
| `AKASHI_AUTO_TRACE` | `true` | Auto-trace git commits as decisions. Set `false` for manual-only tracing. |

## Security model

**Localhost-only by default.** Requests from `127.0.0.1` or `::1` are always allowed
without credentials. Non-loopback requests require a matching `X-Akashi-Hook-Key` header.

This is intentional: hooks fire from the developer's machine during IDE sessions. JWT
authentication would be overkill for a localhost-to-localhost call.

| Scenario | Configuration |
|----------|---------------|
| Local dev (recommended) | `AKASHI_HOOKS_API_KEY=""` — localhost access only, no secret needed |
| Remote IDE (SSH, Codespaces) | Set `AKASHI_HOOKS_API_KEY=<secret>` on both server and client |
| CI/automation | `AKASHI_HOOKS_ENABLED=false` — hooks are not needed |

## Cursor setup

The install script also checks for Cursor configuration. For Cursor, create:

**`.cursor/hooks.json`:**
```json
{
  "hooks": {
    "beforeShellExecution": "~/.claude/hooks/akashi-hook.sh",
    "afterShellExecution": "~/.claude/hooks/akashi-hook.sh",
    "beforeMCPExecution": "~/.claude/hooks/akashi-hook.sh",
    "afterMCPExecution": "~/.claude/hooks/akashi-hook.sh",
    "afterFileEdit": "~/.claude/hooks/akashi-hook.sh"
  }
}
```

The hook script normalizes Cursor event names (`beforeShellExecution` → `PreToolUse`, etc.)
so both IDEs use the same server endpoints.

## Graceful degradation

If the Akashi server is unreachable (curl timeout: 3 seconds):

1. **Session start**: Prints a reminder to stderr, continues normally.
2. **Edit gate**: Falls back to an agent-scoped marker file (`/tmp/akashi-checked-{agent_id}`)
   with the same 10-minute TTL. Edits are still gated, just enforced locally.
3. **Commit trace**: Prints a reminder to call `akashi_trace` manually.

When the server comes back, the in-memory session tracking takes over seamlessly.

## Verifying hooks are working

After installation, start a new IDE session and check:

1. **Session start context** appears with recent decisions and conflict counts.
2. **Attempting an edit** before calling `akashi_check` is blocked with a clear message.
3. **After calling `akashi_check`**, edits proceed normally.
4. **After a `git commit`**, you see either an auto-trace confirmation or a trace reminder.

To test the server endpoint directly:

```bash
curl -s -X POST http://localhost:8080/hooks/session-start \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"test","cwd":"/tmp"}' | jq .
```
