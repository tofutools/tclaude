"""Bound main-only test build snapshots without evicting unrelated caches."""
from datetime import datetime
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

# Caches other workflows save on main and never prune. Left alone they
# filled the 10 GB limit, so no build snapshot could be saved and every
# agentd/sandbox run compiled cold. Each rule maps a key to its family and its
# generation line within that family; only the newest entries of the current
# line are ever restored, so everything else is reclaimable.
#
# - CodeQL (repository default setup) saves an overlay base database per
#   language on every push to main, ~680 MB a commit in all. It restores the
#   newest base of its own CodeQL version, so once a newer version has saved,
#   every older version's bases are dead weight too: the version is the line.
#   Keep two of the current version in case one is being written or was just
#   superseded.
# - CodeQL's TRAP cache is saved per commit: keep the newest two per language,
#   whatever their version.
# - CodeQL's dependency cache and setup-go's module/build cache are keyed by a
#   hash of the dependency files, so a go.mod/go.sum change leaves the
#   previous generation behind. Nothing restores one by prefix: keep only the
#   newest.
OVERLAY = re.compile(r"^(codeql-overlay-base-database-\d+-[0-9a-f]+-[a-z0-9_]+)-([0-9.]+)-[0-9a-f]{40}-\d+-\d+$")
TRAP = re.compile(r"^codeql-trap-(\d+)-[0-9.]+-([a-z0-9_]+)-[0-9a-f]{40}$")
DEPENDENCIES = re.compile(r"^(codeql-dependencies-\d+-[A-Za-z]+-[a-z0-9_]+)-([0-9a-f]{64})$")
# The Go version belongs to the line, not the family: a toolchain bump
# supersedes the previous version's entries just as a go.sum change does.
SETUP_GO = re.compile(r"^(setup-go-\S+)-go-([0-9.]+-[0-9a-f]{64})$")
SUPERSEDABLE = [
    # (pattern, (family, line) of a match, entries kept of the current line)
    (OVERLAY, lambda m: (m.group(1), m.group(2)), 2),
    (TRAP, lambda m: (f"codeql-trap-{m.group(1)}-{m.group(2)}", ""), 2),
    (DEPENDENCIES, lambda m: (m.group(1), m.group(2)), 1),
    (SETUP_GO, lambda m: (m.group(1), m.group(2)), 1),
]


def api(path, *args):
    return subprocess.check_output(["gh", "api", path, *args], text=True)


def caches(repo):
    pages = json.loads(api(f"repos/{repo}/actions/caches?per_page=100", "--paginate", "--slurp"))
    return [entry for page in pages for entry in page["actions_caches"]]


def can_save(entries, size):
    # Account conservatively for every parallel saver before any pruning.
    return size <= MAX_SNAPSHOT and sum(e["size_in_bytes"] for e in entries) + SAVERS * MAX_SNAPSHOT <= LIMIT - HEADROOM


def superseded(entries):
    """Main-branch caches of a SUPERSEDABLE family that nothing restores any
    more. A family's current generation line is that of its newest entry;
    entries of any other line are superseded, and so is any entry of the
    current line past the family's keep count. Nothing else qualifies."""
    stale = []
    for pattern, classify, keep in SUPERSEDABLE:
        families = {}
        for entry in entries:
            match = pattern.match(entry["key"])
            if entry["ref"] == "refs/heads/main" and match:
                family, line = classify(match)
                families.setdefault(family, []).append((line, entry))
        for family in families.values():
            # Parsed, not compared as text: the API's fractional seconds vary in length.
            family.sort(key=lambda g: (created(g[1]), g[1]["id"]), reverse=True)
            current = family[0][0]
            kept = [e for line, e in family if line == current]
            stale += [e for line, e in family if line != current] + kept[keep:]
    return stale


def created(entry):
    """created_at as a datetime. The API's fractional seconds vary in length,
    which Python < 3.11 (macOS runners) cannot parse, so normalise them."""
    stamp, _, fraction = entry["created_at"].rstrip("Z").partition(".")
    micros = (fraction + "000000")[:6]
    return datetime.strptime(stamp, "%Y-%m-%dT%H:%M:%S").replace(microsecond=int(micros))


def delete(repo, entry):
    """Delete one cache entry; a parallel saver may have deleted it first."""
    try:
        api(f"repos/{repo}/actions/caches/{entry['id']}", "--method", "DELETE")
    except subprocess.CalledProcessError:
        if any(e["id"] == entry["id"] for e in caches(repo)):
            raise
        return False
    return True


# Go marks a build-cache entry used by bumping its mtime, but only once the
# mtime is older than this (cmd/go/internal/cache: mtimeInterval), so an
# entry used this run may carry an mtime up to an hour before the run.
GO_CACHE_MTIME_INTERVAL = 3600
# Slack for clock granularity between the marker and the cache's writes.
TRIM_SLACK = 300


def trim_unused(gocache, used_since):
    """Delete the Go build-cache entries this run did not use, so the saved
    snapshot holds the shard's working set rather than everything setup-go
    restored before it. An entry is kept when its mtime is within
    GO_CACHE_MTIME_INTERVAL (+ slack) of used_since: Go would have bumped
    an older one on use. Only the hashed entry files (00/ .. ff/) are
    candidates; the cache's own bookkeeping files stay. Returns
    (kept_bytes, removed_bytes)."""
    cutoff = used_since - GO_CACHE_MTIME_INTERVAL - TRIM_SLACK
    kept = removed = 0
    for sub in Path(gocache).iterdir():
        if not (sub.is_dir() and len(sub.name) == 2 and all(c in "0123456789abcdef" for c in sub.name)):
            continue
        for entry in sub.iterdir():
            if not entry.is_file():
                continue
            info = entry.stat()
            if info.st_mtime < cutoff:
                entry.unlink()
                removed += info.st_size
            else:
                kept += info.st_size
    return kept, removed


def obsolete(entries, prefix, keep):
    return [e for e in entries if e["ref"] == "refs/heads/main" and e["key"].startswith(prefix) and e["key"] != keep]


def main():
    operation, prefix, key = sys.argv[1:]
    if os.environ["GITHUB_REF"] != "refs/heads/main":
        raise SystemExit("build cache mutations require main")
    repo = os.environ["GITHUB_REPOSITORY"]
    entries = caches(repo)
    if operation == "prepare":
        used_since = os.environ.get("BUILD_CACHE_USED_SINCE")
        if used_since:
            kept, removed = trim_unused(os.environ["GOCACHE"], int(used_since))
            print(f"Trimmed the build cache to this run's working set: kept {kept} bytes, removed {removed} unused bytes")
        size = sum(p.stat().st_size for p in Path(os.environ["GOCACHE"]).rglob("*") if p.is_file())
        # Housekeeping, never a reason to fail the release: an entry that
        # could not be deleted simply still counts against the budget.
        gone = set()
        for entry in superseded(entries):
            try:
                if delete(repo, entry):
                    print(f"Deleted superseded cache {entry['key']} ({entry['size_in_bytes']} bytes)")
                gone.add(entry["id"])
            except (subprocess.CalledProcessError, OSError, ValueError) as error:
                print(f"::warning title=cache not pruned::{entry['key']}: {error}")
        entries = [e for e in entries if e["id"] not in gone]
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
            if delete(repo, entry):
                print(f"Deleted previous build snapshot {entry['key']} ({entry['size_in_bytes']} bytes)")
    else:
        raise SystemExit("expected prepare or prune")


if __name__ == "__main__":
    main()
