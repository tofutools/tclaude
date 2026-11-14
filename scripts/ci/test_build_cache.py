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
            overlay("go", "2026-11-13T00:00:00Z", 22, version="2.28.0"),  # its own generation line
            {"id": 31, "ref": "refs/heads/main", "created_at": "2026-11-01T00:00:00Z",
             "key": "codeql-dependencies-1-Linux-go-91b47499f10a"},
            {"id": 32, "ref": "refs/heads/main", "created_at": "2026-11-01T00:00:00Z",
             "key": "setup-go-Linux-x64-ubuntu26-go-1.27.4-668b513fb9c9"},
        ]
        stale = cache.superseded_codeql(go + js + other)
        self.assertEqual(sorted(e["id"] for e in stale), [1, 2])

    def test_creation_times_compare_across_fraction_lengths(self):
        times = ["2026-11-14T19:09:14.764Z", "2026-11-14T19:09:14.7640001Z", "2026-11-14T19:09:14Z",
                 "2026-11-14T19:09:14.76Z"]
        order = sorted(times, key=lambda t: cache.created({"created_at": t}))
        self.assertEqual(order, ["2026-11-14T19:09:14Z", "2026-11-14T19:09:14.76Z",
                                 "2026-11-14T19:09:14.764Z", "2026-11-14T19:09:14.7640001Z"])


if __name__ == "__main__":
    unittest.main()
