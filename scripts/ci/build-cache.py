"""Bound main-only test build snapshots without evicting unrelated caches."""
import json
import os
from pathlib import Path
import subprocess
import sys

LIMIT = 10 * 1024**3
HEADROOM = 512 * 1024**2
MAX_SNAPSHOT = 1024**3
SAVERS = 4  # two OSes times agentd/sandbox in the release matrix


def api(path, *args):
    return subprocess.check_output(["gh", "api", path, *args], text=True)


def caches(repo):
    pages = json.loads(api(f"repos/{repo}/actions/caches?per_page=100", "--paginate", "--slurp"))
    return [entry for page in pages for entry in page["actions_caches"]]


def can_save(entries, size):
    # Account conservatively for all four parallel savers before any pruning.
    return size <= MAX_SNAPSHOT and sum(e["size_in_bytes"] for e in entries) + SAVERS * MAX_SNAPSHOT <= LIMIT - HEADROOM


def obsolete(entries, prefix, keep):
    return [e for e in entries if e["ref"] == "refs/heads/main" and e["key"].startswith(prefix) and e["key"] != keep]


def main():
    operation, prefix, key = sys.argv[1:]
    if os.environ["GITHUB_REF"] != "refs/heads/main":
        raise SystemExit("build cache mutations require main")
    repo = os.environ["GITHUB_REPOSITORY"]
    entries = caches(repo)
    if operation == "prepare":
        size = sum(p.stat().st_size for p in Path(os.environ["GOCACHE"]).rglob("*") if p.is_file())
        allowed = can_save(entries, size)
        print(f"Build cache: {size} bytes on disk; repository caches: {sum(e['size_in_bytes'] for e in entries)} bytes; save={allowed}")
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            output.write(f"save={'true' if allowed else 'false'}\n")
    elif operation == "prune":
        # A failed/no-op save must never discard the last usable snapshot.
        if not any(e["ref"] == "refs/heads/main" and e["key"] == key for e in entries):
            raise SystemExit("new build snapshot not found; retaining old caches")
        for entry in obsolete(entries, prefix, key):
            api(f"repos/{repo}/actions/caches/{entry['id']}", "--method", "DELETE")
            print(f"Deleted previous build snapshot {entry['key']} ({entry['size_in_bytes']} bytes)")
    else:
        raise SystemExit("expected prepare or prune")


if __name__ == "__main__":
    main()
