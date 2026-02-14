#!/usr/bin/env python3
"""Reconcile Postgres current decisions against Qdrant point IDs.

Usage:
  python3 scripts/reconcile_qdrant.py [--repair] [--org-id <uuid>]

Environment:
  DATABASE_URL         required
  QDRANT_URL           required (REST or gRPC URL; :6334 auto-mapped to :6333)
  QDRANT_COLLECTION    optional (default: akashi_decisions)
  QDRANT_API_KEY       optional
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent


def qdrant_rest_base(raw_url: str) -> str:
    parsed = urllib.parse.urlparse(raw_url)
    if not parsed.scheme or not parsed.netloc:
        raise ValueError(f"invalid QDRANT_URL: {raw_url!r}")
    host = parsed.hostname or ""
    port = parsed.port
    scheme = parsed.scheme
    if port is None:
        # Qdrant default REST port.
        port = 6333
    elif port == 6334:
        # User may provide gRPC endpoint; REST scroll uses 6333.
        port = 6333
    return f"{scheme}://{host}:{port}"


def run_psql(database_url: str, sql: str) -> str:
    result = subprocess.run(
        ["psql", database_url, "-At", "-v", "ON_ERROR_STOP=1", "-c", sql],
        capture_output=True,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        raise RuntimeError(result.stderr.strip() or "psql failed")
    return result.stdout


def pg_current_decisions(database_url: str, org_id: str | None) -> dict[str, str]:
    where = "WHERE valid_to IS NULL AND embedding IS NOT NULL"
    if org_id:
        where += f" AND org_id = '{org_id}'::uuid"
    sql = f"SELECT id::text || '|' || org_id::text FROM decisions {where};"
    rows = run_psql(database_url, sql).strip().splitlines()
    out: dict[str, str] = {}
    for row in rows:
        if not row:
            continue
        decision_id, o = row.split("|", 1)
        out[decision_id] = o
    return out


def qdrant_scroll_ids(base_url: str, collection: str, api_key: str, org_id: str | None) -> set[str]:
    headers = {"Content-Type": "application/json"}
    if api_key:
        headers["api-key"] = api_key

    ids: set[str] = set()
    offset = None

    while True:
        body: dict[str, object] = {
            "limit": 1000,
            "with_payload": False,
            "with_vector": False,
        }
        if offset is not None:
            body["offset"] = offset
        if org_id:
            body["filter"] = {"must": [{"key": "org_id", "match": {"value": org_id}}]}

        req = urllib.request.Request(
            f"{base_url}/collections/{collection}/points/scroll",
            data=json.dumps(body).encode("utf-8"),
            headers=headers,
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=20) as resp:
                payload = json.loads(resp.read().decode("utf-8"))
        except urllib.error.HTTPError as e:
            raise RuntimeError(f"qdrant scroll failed: HTTP {e.code}: {e.read().decode('utf-8', errors='ignore')}")

        result = payload.get("result", {})
        points = result.get("points", [])
        for p in points:
            pid = p.get("id")
            if isinstance(pid, str):
                ids.add(pid)

        offset = result.get("next_page_offset")
        if not offset:
            break

    return ids


def enqueue_repairs(database_url: str, missing: list[str], pg_map: dict[str, str]) -> None:
    # Batch inserts to keep SQL payload bounded.
    batch_size = 500
    for i in range(0, len(missing), batch_size):
        batch = missing[i : i + batch_size]
        values = []
        for decision_id in batch:
            org_id = pg_map[decision_id]
            values.append(f"('{decision_id}'::uuid, '{org_id}'::uuid, 'upsert')")
        values_sql = ",\n  ".join(values)
        sql = f"""
INSERT INTO search_outbox (decision_id, org_id, operation)
VALUES
  {values_sql}
ON CONFLICT (decision_id, operation) DO UPDATE
  SET created_at = now(), attempts = 0, locked_until = NULL;
"""
        run_psql(database_url, sql)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repair", action="store_true", help="enqueue missing Postgres decisions into search_outbox")
    parser.add_argument("--org-id", default=None, help="limit reconciliation to one organization UUID")
    args = parser.parse_args()

    database_url = os.environ.get("DATABASE_URL", "")
    qdrant_url = os.environ.get("QDRANT_URL", "")
    collection = os.environ.get("QDRANT_COLLECTION", "akashi_decisions")
    api_key = os.environ.get("QDRANT_API_KEY", "")

    if not database_url:
        print("error: DATABASE_URL is required", file=sys.stderr)
        return 2
    if not qdrant_url:
        print("error: QDRANT_URL is required", file=sys.stderr)
        return 2

    base_url = qdrant_rest_base(qdrant_url)

    pg_map = pg_current_decisions(database_url, args.org_id)
    qdrant_ids = qdrant_scroll_ids(base_url, collection, api_key, args.org_id)

    pg_ids = set(pg_map.keys())
    missing_in_qdrant = sorted(pg_ids - qdrant_ids)
    extra_in_qdrant = sorted(qdrant_ids - pg_ids)

    print(
        json.dumps(
            {
                "postgres_current_count": len(pg_ids),
                "qdrant_count": len(qdrant_ids),
                "missing_in_qdrant": len(missing_in_qdrant),
                "extra_in_qdrant": len(extra_in_qdrant),
                "org_scope": args.org_id,
            },
            indent=2,
        )
    )

    if missing_in_qdrant and args.repair:
        enqueue_repairs(database_url, missing_in_qdrant, pg_map)
        print(f"queued_repairs={len(missing_in_qdrant)}")

    # Non-zero when drift remains unresolved.
    if missing_in_qdrant and not args.repair:
        return 1
    if extra_in_qdrant:
        # extra points require manual/targeted delete policy decisions.
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
