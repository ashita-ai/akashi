#!/usr/bin/env python3
"""Program-level durability exit criteria verifier.

Checks:
  - orphan integrity (decisions/alternatives/evidence)
  - dead-letter threshold
  - oldest pending outbox age threshold
  - optional strict retention check on agent_events
  - optional Postgres vs Qdrant drift check

Exit code:
  0 = all required checks passed
  1 = one or more checks failed
  2 = configuration/runtime error
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import time
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parent.parent


def run_psql(database_url: str, sql: str) -> str:
    result = subprocess.run(
        ["psql", database_url, "-At", "-v", "ON_ERROR_STOP=1", "-c", sql],
        capture_output=True,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        raise RuntimeError(result.stderr.strip() or "psql failed")
    return result.stdout.strip()


def run_reconcile_script(env: dict[str, str]) -> dict[str, Any]:
    script = ROOT / "scripts" / "reconcile_qdrant.py"
    result = subprocess.run(
        [sys.executable, str(script)],
        capture_output=True,
        text=True,
        env=env,
        check=False,
    )

    payload = {}
    for line in result.stdout.splitlines():
        line = line.strip()
        if line.startswith("{") and line.endswith("}"):
            try:
                payload = json.loads(line)
                break
            except json.JSONDecodeError:
                continue
    return {
        "returncode": result.returncode,
        "payload": payload,
        "stdout": result.stdout.strip(),
        "stderr": result.stderr.strip(),
    }


def as_int(value: str, default: int) -> int:
    try:
        return int(value)
    except Exception:
        return default


def as_bool(value: str, default: bool) -> bool:
    v = value.strip().lower()
    if v in {"1", "true", "yes", "y", "on"}:
        return True
    if v in {"0", "false", "no", "n", "off"}:
        return False
    return default


def main() -> int:
    database_url = os.environ.get("DATABASE_URL", "")
    qdrant_url = os.environ.get("QDRANT_URL", "")

    if not database_url:
        print("error: DATABASE_URL is required", file=sys.stderr)
        return 2

    max_dead_letters = as_int(os.environ.get("MAX_DEAD_LETTERS", "0"), 0)
    max_outbox_oldest_seconds = as_int(os.environ.get("MAX_OUTBOX_OLDEST_SECONDS", "1800"), 1800)
    retain_days = as_int(os.environ.get("RETAIN_DAYS", "90"), 90)
    strict_retention = as_bool(os.environ.get("STRICT_RETENTION_CHECK", "false"), False)

    checks: list[dict[str, Any]] = []
    started_at = int(time.time())

    try:
        orphan_raw = run_psql(
            database_url,
            """
            SELECT
              (SELECT count(*) FROM decisions d LEFT JOIN agent_runs r ON r.id = d.run_id WHERE r.id IS NULL),
              (SELECT count(*) FROM alternatives a LEFT JOIN decisions d ON d.id = a.decision_id WHERE d.id IS NULL),
              (SELECT count(*) FROM evidence e LEFT JOIN decisions d ON d.id = e.decision_id WHERE d.id IS NULL);
            """,
        )
        d_wo_run, a_wo_decision, e_wo_decision = [int(x) for x in orphan_raw.split("|")]
        orphan_total = d_wo_run + a_wo_decision + e_wo_decision
        checks.append(
            {
                "name": "orphan_integrity",
                "passed": orphan_total == 0,
                "details": {
                    "decisions_without_run": d_wo_run,
                    "alternatives_without_decision": a_wo_decision,
                    "evidence_without_decision": e_wo_decision,
                },
            }
        )

        dead_letter_raw = run_psql(
            database_url,
            "SELECT count(*) FROM search_outbox WHERE attempts >= 10;",
        )
        dead_letters = int(dead_letter_raw or "0")
        checks.append(
            {
                "name": "dead_letter_threshold",
                "passed": dead_letters <= max_dead_letters,
                "details": {"dead_letters": dead_letters, "max_allowed": max_dead_letters},
            }
        )

        oldest_raw = run_psql(
            database_url,
            """
            SELECT COALESCE(EXTRACT(EPOCH FROM (now() - min(created_at)))::bigint, 0)
            FROM search_outbox
            WHERE attempts < 10;
            """,
        )
        oldest_seconds = int(oldest_raw or "0")
        checks.append(
            {
                "name": "outbox_oldest_age",
                "passed": oldest_seconds <= max_outbox_oldest_seconds,
                "details": {
                    "oldest_pending_seconds": oldest_seconds,
                    "max_allowed_seconds": max_outbox_oldest_seconds,
                },
            }
        )

        if strict_retention:
            old_events_raw = run_psql(
                database_url,
                f"""
                SELECT count(*)
                FROM agent_events
                WHERE occurred_at < now() - ('{retain_days} days')::interval;
                """,
            )
            old_events = int(old_events_raw or "0")
            checks.append(
                {
                    "name": "strict_retention_window",
                    "passed": old_events == 0,
                    "details": {"events_older_than_retain_days": old_events, "retain_days": retain_days},
                }
            )
        else:
            checks.append(
                {
                    "name": "strict_retention_window",
                    "passed": True,
                    "skipped": True,
                    "details": {"reason": "STRICT_RETENTION_CHECK=false"},
                }
            )

        if qdrant_url:
            recon = run_reconcile_script(dict(os.environ))
            payload = recon.get("payload", {})
            missing = int(payload.get("missing_in_qdrant", 0)) if payload else None
            extra = int(payload.get("extra_in_qdrant", 0)) if payload else None
            passed = recon["returncode"] == 0 and missing == 0 and extra == 0
            checks.append(
                {
                    "name": "qdrant_reconciliation",
                    "passed": passed,
                    "details": {
                        "missing_in_qdrant": missing,
                        "extra_in_qdrant": extra,
                        "returncode": recon["returncode"],
                        "stderr": recon["stderr"],
                    },
                }
            )
        else:
            checks.append(
                {
                    "name": "qdrant_reconciliation",
                    "passed": True,
                    "skipped": True,
                    "details": {"reason": "QDRANT_URL not set"},
                }
            )

    except Exception as e:
        print(json.dumps({"error": str(e)}, indent=2), file=sys.stderr)
        return 2

    all_passed = all(bool(c.get("passed")) for c in checks)
    summary = {
        "started_at_unix": started_at,
        "completed_at_unix": int(time.time()),
        "all_passed": all_passed,
        "checks": checks,
    }
    print(json.dumps(summary, indent=2))
    return 0 if all_passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
