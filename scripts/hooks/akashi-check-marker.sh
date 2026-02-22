#!/bin/bash
# Claude Code PostToolUse hook.
# Fires after akashi_check or akashi_trace MCP tool calls.
# Writes a marker file so akashi-precheck-gate.sh knows a check is on record
# and allows subsequent Edit/Write/MultiEdit calls to proceed.
#
# Installed by: make install-hooks
# Registered in: ~/.claude/settings.json

touch "/tmp/akashi-checked-$(whoami)"
