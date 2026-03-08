#!/usr/bin/env bash
# check_coverage.sh — Enforce minimum test coverage from a Go coverage profile.
#
# Usage: scripts/check_coverage.sh coverage.out [threshold] [exclude_pattern]
#   coverage.out     Path to the Go coverage profile (from -coverprofile)
#   threshold        Minimum total coverage percentage (default: 50)
#   exclude_pattern  Grep -E pattern for packages to exclude (default: cmd/)
#
# Excludes entrypoint packages (cmd/) that contain main() and aren't
# meaningfully unit-testable. The overall total reported by `go tool cover`
# includes these at 0%, which drags down the aggregate unfairly.
#
# Exit codes:
#   0  Coverage meets or exceeds threshold
#   1  Coverage below threshold or usage error

set -euo pipefail

COVERAGE_FILE="${1:-}"
THRESHOLD="${2:-50}"
EXCLUDE_PATTERN="${3:-/cmd/}"

if [[ -z "$COVERAGE_FILE" ]]; then
    echo "usage: $0 <coverage.out> [threshold] [exclude_pattern]" >&2
    exit 1
fi

if [[ ! -f "$COVERAGE_FILE" ]]; then
    echo "error: coverage file not found: $COVERAGE_FILE" >&2
    exit 1
fi

# Filter out excluded packages from the coverage profile, then compute total.
# The profile header (mode: line) must be preserved for `go tool cover`.
FILTERED=$(mktemp)
trap 'rm -f "$FILTERED"' EXIT
head -1 "$COVERAGE_FILE" > "$FILTERED"
tail -n +2 "$COVERAGE_FILE" | grep -v -E "$EXCLUDE_PATTERN" >> "$FILTERED" || true

TOTAL_LINE=$(go tool cover -func="$FILTERED" | tail -1)
COVERAGE=$(echo "$TOTAL_LINE" | awk '{print $NF}' | tr -d '%')

if [[ -z "$COVERAGE" ]]; then
    echo "error: could not parse coverage from $COVERAGE_FILE" >&2
    exit 1
fi

echo "Total coverage: ${COVERAGE}% (threshold: ${THRESHOLD}%, excluding: ${EXCLUDE_PATTERN})"

# Compare as floating point using awk (bash can't do float comparison).
PASS=$(awk "BEGIN {print ($COVERAGE >= $THRESHOLD) ? 1 : 0}")

if [[ "$PASS" -eq 1 ]]; then
    echo "PASS: coverage ${COVERAGE}% >= ${THRESHOLD}%"
    exit 0
else
    echo "FAIL: coverage ${COVERAGE}% < ${THRESHOLD}%" >&2
    exit 1
fi
