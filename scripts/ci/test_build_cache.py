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


if __name__ == "__main__":
    unittest.main()
