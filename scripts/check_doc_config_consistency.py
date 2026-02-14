#!/usr/bin/env python3
"""Validate environment variable consistency between config and docs.

Usage:
  python3 scripts/check_doc_config_consistency.py
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent
MANIFEST_PATH = ROOT / "docs" / "consistency-manifest.json"


def read_text(path: Path) -> str:
    return path.read_text(encoding="utf-8")


def extract_source_vars(config_go: str) -> set[str]:
    patterns = [
        r'envStr\("([A-Z][A-Z0-9_]+)"',
        r'envStrSlice\("([A-Z][A-Z0-9_]+)"',
        r'collectInt\(errs, "([A-Z][A-Z0-9_]+)"',
        r'collectFloat64\(errs, "([A-Z][A-Z0-9_]+)"',
        r'collectBool\(errs, "([A-Z][A-Z0-9_]+)"',
        r'collectDuration\(errs, "([A-Z][A-Z0-9_]+)"',
    ]
    vars_found: set[str] = set()
    for pattern in patterns:
        vars_found.update(re.findall(pattern, config_go))
    return vars_found


def extract_doc_vars(md_text: str) -> set[str]:
    # Most env vars in docs are rendered in backticks.
    return set(re.findall(r"`([A-Z][A-Z0-9_]+)`", md_text))


def extract_env_example_vars(env_text: str) -> set[str]:
    vars_found: set[str] = set()
    for line in env_text.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        match = re.match(r"^([A-Z][A-Z0-9_]+)=", stripped)
        if match:
            vars_found.add(match.group(1))
    return vars_found


def main() -> int:
    if not MANIFEST_PATH.exists():
        print(f"error: manifest not found: {MANIFEST_PATH}", file=sys.stderr)
        return 2

    manifest = json.loads(read_text(MANIFEST_PATH))

    source_path = ROOT / manifest["source"]["path"]
    source_vars = extract_source_vars(read_text(source_path))

    failures: list[str] = []

    for target in manifest.get("targets", []):
        target_path = ROOT / target["path"]
        target_vars = extract_doc_vars(read_text(target_path))

        rule = target.get("rule", "must_include_all_source_vars")
        if rule == "must_include_all_source_vars":
            required_vars = source_vars
        elif rule == "must_include_vars":
            required_vars = set(target.get("required_vars", []))
        else:
            failures.append(f"{target['name']} has unknown rule: {rule}")
            continue

        missing = sorted(required_vars - target_vars)
        if missing:
            failures.append(
                f"{target['name']} missing {len(missing)} vars:\n  - " + "\n  - ".join(missing)
            )

        if not target.get("allow_extra", True):
            extras = sorted(target_vars - source_vars)
            if extras:
                failures.append(
                    f"{target['name']} has {len(extras)} extra vars:\n  - " + "\n  - ".join(extras)
                )

    env_cfg = manifest.get("env_example", {})
    if env_cfg:
        env_path = ROOT / env_cfg["path"]
        env_vars = extract_env_example_vars(read_text(env_path))
        required = set(env_cfg.get("required_vars", []))
        missing_required = sorted(required - env_vars)
        if missing_required:
            failures.append(
                f".env.example missing {len(missing_required)} required vars:\n  - "
                + "\n  - ".join(missing_required)
            )

    if failures:
        print("Doc/config consistency check FAILED\n")
        for i, failure in enumerate(failures, start=1):
            print(f"{i}) {failure}\n")
        return 1

    print(
        "Doc/config consistency check passed:\n"
        f"- source vars: {len(source_vars)}\n"
        f"- docs checked: {len(manifest.get('targets', []))}\n"
        f"- manifest: {manifest['version']}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
