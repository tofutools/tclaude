"""Bound main-only test build snapshots without evicting unrelated caches."""
import json
import os
import re
from pathlib import Path
import subprocess
import sys

LIMIT = 10 * 1024**3
HEADROOM = 512 * 1024**2
MAX_SNAPSHOT = 1024**3
SAVERS = 3  # release.yml: Linux agentd + sandbox shards, macOS agentd

# CodeQL (repository default setup) saves an overlay base database per
# language on every push to main, ~680 MB a commit in all, and never prunes
# them: the cache sat at its 10 GB limit with a dozen generations, so no
# build snapshot could ever be saved and every agentd/sandbox run compiled
# cold. CodeQL restores the newest base by key prefix, so only the newest
# generation per prefix is used. Keep two in case one is being written or
# was just superseded; anything older is reclaimable.
CODEQL_OVERLAY = re.compile(r"^(codeql-overlay-base-database-\d+-[0-9a-f]+-[a-z0-9_]+-[0-9.]+)-[0-9a-f]{40}-\d+-\d+$")
CODEQL_KEEP = 2


def api(path, *args):
    return subprocess.check_output(["gh", "api", path, *args], text=True)


def caches(repo):
    pages = json.loads(api(f"repos/{repo}/actions/caches?per_page=100", "--paginate", "--slurp"))
    return [entry for page in pages for entry in page["actions_caches"]]


def can_save(entries, size):
    # Account conservatively for every parallel saver before any pruning.
    return size <= MAX_SNAPSHOT and sum(e["size_in_bytes"] for e in entries) + SAVERS * MAX_SNAPSHOT <= LIMIT - HEADROOM


def superseded_codeql(entries):
    """CodeQL overlay base databases on main older than the newest
    CODEQL_KEEP of their language/configuration. Nothing else qualifies."""
    groups = {}
    for entry in entries:
        match = CODEQL_OVERLAY.match(entry["key"])
        if entry["ref"] == "refs/heads/main" and match:
            groups.setdefault(match.group(1), []).append(entry)
    stale = []
    for group in groups.values():
        group.sort(key=lambda e: (e["created_at"], e["id"]), reverse=True)
        stale += group[CODEQL_KEEP:]
    return stale


def delete(repo, entry):
    """Delete one cache entry; a parallel saver may have deleted it first."""
    try:
        api(f"repos/{repo}/actions/caches/{entry['id']}", "--method", "DELETE")
    except subprocess.CalledProcessError:
        if any(e["id"] == entry["id"] for e in caches(repo)):
            raise
        return False
    return True


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
        stale = superseded_codeql(entries)
        for entry in stale:
            if delete(repo, entry):
                print(f"Deleted superseded CodeQL overlay base {entry['key']} ({entry['size_in_bytes']} bytes)")
        stale_ids = {e["id"] for e in stale}
        entries = [e for e in entries if e["id"] not in stale_ids]
        allowed = can_save(entries, size)
        print(f"Build cache: {size} bytes on disk; repository caches: {sum(e['size_in_bytes'] for e in entries)} bytes; save={allowed}")
        if not allowed:
            # Silent refusals left every run compiling cold for weeks.
            print(f"::warning title=build cache not saved::{size} byte snapshot does not fit: "
                  f"{sum(e['size_in_bytes'] for e in entries)} bytes of repository caches, "
                  f"limit {LIMIT}, room reserved for {SAVERS} snapshots of up to {MAX_SNAPSHOT}")
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            output.write(f"save={'true' if allowed else 'false'}\n")
    elif operation == "prune":
        # A failed/no-op save must never discard the last usable snapshot.
        if not any(e["ref"] == "refs/heads/main" and e["key"] == key for e in entries):
            raise SystemExit("new build snapshot not found; retaining old caches")
        for entry in obsolete(entries, prefix, key):
            delete(repo, entry)
            print(f"Deleted previous build snapshot {entry['key']} ({entry['size_in_bytes']} bytes)")
    else:
        raise SystemExit("expected prepare or prune")


if __name__ == "__main__":
    main()
