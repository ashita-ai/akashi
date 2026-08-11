#!/usr/bin/env python3
"""Conflict detection by exact join — no language model in the loop.

Two agents set the same named parameter to different values. That is not a
language problem, it is a lookup: same org, same project, both decisions
current, different value. 27% of the real contradictions in our corpus have
this shape, and the join finds them at zero LLM cost with no false-positive
rate at all.

Contrast with prose conflict detection, which at a 3.35% base rate tops out
around 41% precision even with a strong judge (see base_rate.py).

Prerequisites:
    docker compose -f docker-compose.complete.yml up -d
    export AKASHI_TOKEN=<a JWT for your instance>

Run:
    python examples/python/binding_collision.py
"""

from __future__ import annotations  # keeps `dict | None` working on Python 3.7+

import json
import os
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("AKASHI_URL", "http://localhost:8081")
TOKEN = os.environ.get("AKASHI_TOKEN", "")
PARAM = "conflict_llm_timeout"


def call(method: str, path: str, body: dict | None = None) -> dict:
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method)
    req.add_header("Authorization", f"Bearer {TOKEN}")
    if data:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return json.loads(r.read())
    except urllib.error.HTTPError as e:
        sys.exit(f"{method} {path} -> {e.code}: {e.read().decode()[:200]}")


def trace(agent: str, outcome: str, value: str) -> str:
    """Record one decision that binds PARAM to value."""
    resp = call(
        "POST",
        "/v1/trace",
        {
            "agent_id": agent,
            "decision": {
                "decision_type": "architecture",
                "outcome": outcome,
                "confidence": 0.8,
                "bindings": [{"parameter": PARAM, "value": value}],
            },
        },
    )
    return resp["data"]["decision_id"]


def main() -> None:
    if not TOKEN:
        sys.exit("Set AKASHI_TOKEN to a JWT for your instance.")

    print(f"akashi at {BASE}\n")

    a = trace(
        "timeout-agent-a",
        "Set the conflict judge timeout to 15s — enough for chat-completion models.",
        "15s",
    )
    print(f"  agent-a  {PARAM} = 15s   ({a[:8]})")

    b = trace(
        "timeout-agent-b",
        "Set the conflict judge timeout to 120s — reasoning models need far longer.",
        "120s",
    )
    print(f"  agent-b  {PARAM} = 120s  ({b[:8]})")

    # Scoring runs inline on trace; the join needs no model call, so this is fast.
    print("\nlooking for the collision...")
    for _ in range(20):
        found = [
            c
            for c in call("GET", "/v1/conflicts?status=open&limit=50")["data"]
            if b in (c.get("decision_a_id"), c.get("decision_b_id"))
        ]
        if found:
            c = found[0]
            print("\nCONFLICT")
            print(f"  method:      {c.get('scoring_method', '(n/a)')}")
            print(f"  explanation: {c.get('explanation', '')}")
            print(
                "\nThat sentence was generated from a join, not a judge.\n"
                "No tokens were spent deciding it, and it cannot be a false positive:\n"
                "the two values are either equal or they are not."
            )
            return
        time.sleep(1)

    print("No collision found. Is conflict scoring enabled on this instance?")


if __name__ == "__main__":
    main()
