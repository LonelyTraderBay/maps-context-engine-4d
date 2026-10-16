#!/usr/bin/env python3
"""Collect fresh GitHub sources and verify one published Git delivery."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from tools.spec_check import github_observer
from tools.spec_check.archive import ArchiveError
from tools.spec_check.github_observer import GitHubObserverError, verify_live_delivery
from tools.spec_check.schema import SchemaError


def _retain_blocker(delivery_id: str, exc: BaseException) -> str | None:
    output_root = github_observer._LAST_INVOCATION_ROOT
    if output_root is None or not output_root.is_dir():
        return None
    reason = github_observer._redact(str(exc).encode("utf-8", "replace")).decode("utf-8", "replace")[:4000]
    record = {
        "format_version": 1,
        "kind": "live_verification_blocker",
        "delivery_id": delivery_id,
        "invocation_id": output_root.name,
        "result": "UNKNOWN",
        "reason": reason,
        "recorded_at_utc": github_observer._utc_now(),
    }
    try:
        github_observer._write_member(
            output_root, "blocker.json",
            json.dumps(record, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8"),
        )
    except (OSError, GitHubObserverError):
        return None
    return f"artifacts/git-live/{delivery_id}/{output_root.name}/blocker.json"


def main(argv: list[str] | None = None) -> int:
    # argparse renders Vietnamese help text; Windows consoles may default to
    # CP1252, which otherwise raises UnicodeEncodeError before the CLI starts.
    for stream in (sys.stdout, sys.stderr):
        reconfigure = getattr(stream, "reconfigure", None)
        if callable(reconfigure):
            try:
                reconfigure(encoding="utf-8", errors="backslashreplace")
            except (OSError, ValueError):
                pass
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=ROOT)
    parser.add_argument("--delivery", required=True)
    parser.add_argument("--publication", type=Path, required=True)
    parser.add_argument("--gh", default="gh", help="đường dẫn executable GitHub CLI")
    args = parser.parse_args(argv)
    try:
        result = verify_live_delivery(
            args.root, args.delivery, args.publication, gh=args.gh,
        )
    except (ArchiveError, GitHubObserverError, OSError, SchemaError, ValueError) as exc:
        blocker_ref = _retain_blocker(args.delivery, exc)
        safe_error = github_observer._redact(str(exc).encode("utf-8", "replace")).decode("utf-8", "replace")[:4000]
        payload = {"result": "UNKNOWN", "error": safe_error}
        if blocker_ref is not None:
            payload["blocker_ref"] = blocker_ref
        print(json.dumps(payload, ensure_ascii=False, sort_keys=True), file=sys.stderr)
        return 1
    print(json.dumps(result, ensure_ascii=False, sort_keys=True, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
