import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("build_cache", Path(__file__).with_name("build-cache.py"))
cache = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cache)


class CachePolicyTests(unittest.TestCase):
    def test_parallel_saves_reserve_room_and_reject_oversized_snapshots(self):
        self.assertTrue(cache.can_save([], cache.MAX_SNAPSHOT))
        self.assertFalse(cache.can_save([], cache.MAX_SNAPSHOT + 1))
        occupied = cache.LIMIT - cache.HEADROOM - cache.SAVERS * cache.MAX_SNAPSHOT
        self.assertTrue(cache.can_save([{"size_in_bytes": occupied}], 1))
        self.assertFalse(cache.can_save([{"size_in_bytes": occupied + 1}], 1))

    def test_pruning_preserves_current_other_shards_and_pr_refs(self):
        entries = [
            {"ref": "refs/heads/main", "key": "ours-new"},
            {"ref": "refs/heads/main", "key": "ours-old"},
            {"ref": "refs/heads/main", "key": "smoke-old"},
            {"ref": "refs/pull/12/merge", "key": "ours-old"},
        ]
        self.assertEqual(cache.obsolete(entries, "ours-", "ours-new"), [entries[1]])

    def test_only_codeql_overlay_generations_past_the_newest_two_are_superseded(self):
        sha = "0123456789abcdef0123456789abcdef01234567"
        def overlay(lang, created, ident, ref="refs/heads/main", version="2.27.4"):
            return {"id": ident, "ref": ref, "created_at": created,
                    "key": f"codeql-overlay-base-database-1-433570c23e836bda-{lang}-{version}-{sha}-4364{ident}-1"}
        go = [overlay("go", f"2026-11-14T0{i}:00:00Z", i) for i in range(1, 5)]
        js = [overlay("javascript", "2026-11-14T01:00:00Z", 11), overlay("javascript", "2026-11-14T02:00:00Z", 12)]
        other = [
            overlay("go", "2026-11-13T00:00:00Z", 21, ref="refs/pull/7/merge"),  # not main
            {"id": 31, "ref": "refs/heads/main", "created_at": "2026-11-01T00:00:00Z",
             "key": "gobuild-tests-v1-Linux-X64-agentd-go1.27.4-905cb2c9-race-c598278"},  # snapshots prune themselves
        ]
        stale = cache.superseded(go + js + other)
        self.assertEqual(sorted(e["id"] for e in stale), [1, 2])

    def test_a_newer_codeql_version_supersedes_every_older_overlay_base(self):
        # Real keys from the repository cache listing of 2026-11-26: CodeQL
        # moved 2.27.4 -> 2.27.5 -> 2.27.6, and the old versions' bases sat
        # there (keep-two per version), ~3 GB of the 10 GB limit.
        def overlay(version, sha, run, created, ident):
            return {"id": ident, "ref": "refs/heads/main", "created_at": created,
                    "key": f"codeql-overlay-base-database-1-c801913f1ee29663-javascript-{version}-{sha}-{run}-1"}
        entries = [
            overlay("2.27.4", "5486adde0008547652b3659c57ff13e83be60b0f", 44488889332, "2026-11-19T08:39:32.040293Z", 1),
            overlay("2.27.4", "67f331678997d4baac4e6f77a43a1ba1b27d6d8a", 44635996990, "2026-11-19T23:19:02.948213Z", 2),
            overlay("2.27.5", "73759c4286aca17b4f0af79386e71dcbf444b62f", 45578719075, "2026-11-25T08:47:37.570791Z", 3),
            overlay("2.27.5", "e09d9624cc2713f1db8ab4d982100eb2c70ba7de", 45571379754, "2026-11-25T07:54:47.378658Z", 4),
            overlay("2.27.6", "d637c70122abb16c41211ce6002bafba94a713ee", 45608628406, "2026-11-25T11:56:42.121602Z", 5),
        ]
        self.assertEqual(sorted(e["id"] for e in cache.superseded(entries)), [1, 2, 3, 4])

    def test_dependency_hashed_caches_keep_only_the_newest_generation(self):
        old, new = "668b513fb9c92a64fc7db35ea7ea967a07778679f380ca8ba9ee7184b1632bd8", \
            "ccc68effba0dca5f7f61cc40a14a333a0894a0eeb2e22282533bf4bec00599cb"
        def entry(key, created, ident, ref="refs/heads/main"):
            return {"id": ident, "ref": ref, "created_at": created, "key": key}
        entries = [
            entry(f"setup-go-macOS-arm64-go-1.27.4-{old}", "2026-11-11T20:23:48.521275Z", 1),
            entry(f"setup-go-macOS-arm64-go-1.27.4-{new}", "2026-11-25T12:18:06.350307Z", 2),
            entry(f"setup-go-Linux-x64-ubuntu26-go-1.27.4-{old}", "2026-11-11T19:54:00.665467Z", 3),
            entry(f"setup-go-Linux-x64-ubuntu26-go-1.27.4-{new}", "2026-11-25T11:54:31.606525Z", 4),
            entry(f"setup-go-Linux-x64-ubuntu24-go-1.27.4-{old}", "2026-11-11T19:59:01.484596Z", 5),  # own platform, newest
            entry(f"setup-go-Linux-x64-ubuntu26-go-1.27.4-{old}", "2026-11-01T00:00:00Z", 6, ref="refs/pull/7/merge"),
            entry("codeql-dependencies-1-Linux-go-91b47499f10a128dd753346c83fc37efdc53b5451c5bc1e7cbe77bcdd2056954",
                  "2026-11-08T17:27:12.776897Z", 7),
            entry("codeql-dependencies-1-Linux-go-fc1d9cb84fb8a456eea98bffaa7aa4987033ab2b6f7eda28ed9a1f7039ec185d",
                  "2026-11-25T12:03:58.677767Z", 8),
            entry("codeql-trap-1-2.27.6-javascript-19453f7d7dff7c0b910fee5841e2ee368b170680", "2026-11-26T08:03:23.81018Z", 9),
            entry("codeql-trap-1-2.27.6-javascript-5c0504c0dd19825782dcd5ce49f5b256b5e67a9d", "2026-11-26T07:30:42.228103Z", 10),
            entry("codeql-trap-1-2.27.6-javascript-d637c70122abb16c41211ce6002bafba94a713ee", "2026-11-25T23:53:07.34854Z", 11),
            entry("golangci-lint.cache-Linux-2968-1e6b1fca3eb97d76e3c207bb29fe75f90b774cfa", "2026-11-25T11:57:26Z", 12),
            # A Go toolchain bump supersedes the previous version's entry too.
            entry(f"setup-go-Linux-arm64-ubuntu24-arm64-go-1.27.4-{new}", "2026-11-25T11:56:46.944531Z", 13),
            entry(f"setup-go-Linux-arm64-ubuntu24-arm64-go-1.27.5-{new}", "2026-11-27T11:56:46.944531Z", 14),
        ]
        self.assertEqual(sorted(e["id"] for e in cache.superseded(entries)), [1, 3, 7, 11, 13])

    def test_trim_keeps_this_runs_working_set_and_bookkeeping(self):
        import os, tempfile
        with tempfile.TemporaryDirectory() as root:
            since = 1_800_000_000
            def put(rel, age, size=10):
                path = Path(root, rel)
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(b"x" * size)
                os.utime(path, (since - age, since - age))
                return path
            used_now = put("0a/aa-d", -60)          # written or bumped during the run
            used_recent = put("0b/bb-a", 3000)      # under an hour old: Go does not bump it on use
            stale = put("0c/cc-d", 3 * 3600, 7)     # restored, never used this run
            trim = put("trim.txt", 30 * 86400)      # bookkeeping, whatever its age
            other = put("fuzz/x", 30 * 86400)       # not a hashed entry directory
            kept, removed = cache.trim_unused(root, since)
            self.assertTrue(used_now.exists() and used_recent.exists())
            self.assertFalse(stale.exists())
            self.assertTrue(trim.exists() and other.exists())
            self.assertEqual((kept, removed), (20, 7))

    def test_creation_times_compare_across_fraction_lengths(self):
        times = ["2026-11-14T19:09:14.764Z", "2026-11-14T19:09:14.7640001Z", "2026-11-14T19:09:14Z",
                 "2026-11-14T19:09:14.76Z"]
        order = sorted(times, key=lambda t: cache.created({"created_at": t}))
        self.assertEqual(order, ["2026-11-14T19:09:14Z", "2026-11-14T19:09:14.76Z",
                                 "2026-11-14T19:09:14.764Z", "2026-11-14T19:09:14.7640001Z"])


if __name__ == "__main__":
    unittest.main()
