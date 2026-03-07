#!/usr/bin/env bash
# check_coverage.sh — Enforce minimum test coverage from a Go coverage profile.
#
# Usage: scripts/check_coverage.sh coverage.out [threshold]
#   coverage.out  Path to the Go coverage profile (from -coverprofile)
#   threshold     Minimum total coverage percentage (default: 60)
#
# Exit codes:
#   0  Coverage meets or exceeds threshold
#   1  Coverage below threshold or usage error

set -euo pipefail

COVERAGE_FILE="${1:-}"
THRESHOLD="${2:-60}"

if [[ -z "$COVERAGE_FILE" ]]; then
    echo "usage: $0 <coverage.out> [threshold]" >&2
    exit 1
fi

if [[ ! -f "$COVERAGE_FILE" ]]; then
    echo "error: coverage file not found: $COVERAGE_FILE" >&2
    exit 1
fi

# Extract total coverage percentage from the last line of `go tool cover -func`.
# The last line looks like: "total:	(statements)	72.3%"
TOTAL_LINE=$(go tool cover -func="$COVERAGE_FILE" | tail -1)
COVERAGE=$(echo "$TOTAL_LINE" | awk '{print $NF}' | tr -d '%')

if [[ -z "$COVERAGE" ]]; then
    echo "error: could not parse coverage from $COVERAGE_FILE" >&2
    exit 1
fi

echo "Total coverage: ${COVERAGE}% (threshold: ${THRESHOLD}%)"

# Compare as floating point using awk (bash can't do float comparison).
PASS=$(awk "BEGIN {print ($COVERAGE >= $THRESHOLD) ? 1 : 0}")

if [[ "$PASS" -eq 1 ]]; then
    echo "PASS: coverage ${COVERAGE}% >= ${THRESHOLD}%"
    exit 0
else
    echo "FAIL: coverage ${COVERAGE}% < ${THRESHOLD}%" >&2
    exit 1
fi
