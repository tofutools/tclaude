#!/usr/bin/env python3
"""Readable, non-interleaved failure reports for the agentd test processes."""
import json
import sys
from collections import defaultdict
from pathlib import Path


def report(path, status):
    output = defaultdict(list)
    failed = []
    raw = []
    summaries = []
    for line in Path(path).read_text().splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            raw.append(line)
            continue
        if not isinstance(event, dict) or "Action" not in event:
            raw.append(line)
            continue
        name = event.get("Test", "")
        if event.get("Action") == "output":
            output[name].append(event.get("Output", ""))
        if event.get("Action") == "fail" and name:
            failed.append(name)
        if event.get("Action") in ("pass", "fail") and not name:
            summaries.append(f"{event['Action'].upper()} ({event.get('Elapsed', 0):.3f}s)")
    side = Path(path).stem
    print(f"=== agentd {side}: exit {status} ===")
    for summary in summaries:
        print(summary)
    if status:
        for name in failed:
            print(f"--- failed test: {name} ---")
            print("".join(output[name]), end="")
        if not failed:
            print("No named test failure: build, setup, or process failed.")
        print("".join(output[""]), end="")
        for line in raw:
            print(line)
        print(f"Failing tests ({side}): " + (", ".join(failed) or "(see build/setup output)"))
    elif not summaries:
        # A test process returning success without a package result is suspicious.
        raise ValueError(f"{side}: missing package result")


def main():
    args = sys.argv[1:]
    if not args or len(args) % 2:
        raise SystemExit("usage: agentd-test-report.py LOG STATUS [LOG STATUS ...]")
    invalid = False
    for path, status in zip(args[::2], args[1::2]):
        try:
            report(path, int(status))
        except (ValueError, OSError) as error:
            print(error, file=sys.stderr)
            invalid = True
    if invalid:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
