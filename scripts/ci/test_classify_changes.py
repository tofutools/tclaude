"""Exhaustive tests for classify-changes.py, the PR CI job gate.

Three layers: the pure decision table (every rule, both sides of it), a
monotonicity property (adding a changed path can never make CI run less),
the real git plumbing in a throwaway repository, and consistency checks that
pin the classifier to ci.yml and the test-suite action it drives.
"""
import importlib.util
import itertools
import json
import os
import pathlib
import random
import re
import shutil
import subprocess
import sys
import tempfile
import unittest

HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parent.parent
SCRIPT = HERE / "classify-changes.py"

# Importing the module must not leave a __pycache__ in the checkout.
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("classify_changes", SCRIPT)
cc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cc)

MODES = ("docs", "ui", "code", "full")
LINUX = cc.LINUX
MACOS = cc.MACOS
MAC_ENTRIES = [{"os": MACOS, "shard": s} for s in cc.MACOS_SHARDS]
LINUX_ENTRIES = [{"os": LINUX, "shard": s} for s in cc.LINUX_SHARDS]

# A small fake tree: plain Go, platform Go, a platform package, and files
# whose platform-ness is only visible in their content.
SOURCES = {
    "pkg/claude/agentd/handlers.go": "package agentd\n",
    "pkg/claude/agentd/tray_darwin.go": "package agentd\n",
    "pkg/claude/agentd/tagged.go": "//go:build darwin\n\npackage agentd\n",
    "pkg/claude/agentd/oldtag.go": "// +build linux\n\npackage agentd\n",
    "pkg/claude/agentd/integration.go": "//go:build integration\n\npackage agentd\n",
    "pkg/claude/agentd/cgo.go": 'package agentd\n\nimport "C"\n',
    "pkg/claude/agentd/cgo_group.go": 'package agentd\n\nimport (\n\t"C"\n)\n',
    "pkg/claude/agentd/cgotag.go": "//go:build cgo\n\npackage agentd\n",
    "pkg/claude/agentd/notunix.go": "//go:build !unix\n\npackage agentd\n",
    "pkg/claude/agentd/dashboard_tagged_test.go": "//go:build linux\n\npackage agentd\n",
    "pkg/hostmetrics/read.go": "package hostmetrics\n",
    "pkg/claude/setup/setup.go": 'package setup\n\nfunc f() bool { return runtime.GOOS == "darwin" }\n',
    "pkg/claude/setup/other.go": "package setup\n",
    "pkg/hostmetrics/read_darwin.go": "package hostmetrics\n",
    "pkg/claude/common/db/db.go": "package db\n",
    "pkg/claude/harness/harness.go": "package harness\n",
    "pkg/claude/harness/opencode_sandbox_darwin_test.go": "package harness\n",
    "pkg/claude/harness/pifixture/pi.go": "package pifixture\n",
    "pkg/claude/agentd/jstest/node_test.go": "package jstest\n",
    "pkg/claude/agentd/jstest/suite_memory_other_test.go":
        "//go:build !linux && !darwin\n\npackage jstest\n",
    "pkg/claude/agentd/dashboard/assets.go": "package dashboard\n",
}
GO_FILES = [p for p in SOURCES if p.endswith(".go")]


def read(path):
    return SOURCES.get(path, "package x\n" if path.endswith(".go") else "")


def never(pkg):
    raise AssertionError(f"affected({pkg}) must not be consulted here")


def classify(paths, affected=lambda pkg: False, reader=read):
    return cc.classify(paths, reader, GO_FILES, affected)


class PathPredicates(unittest.TestCase):
    def test_is_doc(self):
        for p in ("docs/x.md", "docs/assets/a.png", "docs/a name\nwith newline.png",
                  "README.md", "CLAUDE.md", "mkdocs.yml"):
            self.assertTrue(cc.is_doc(p), p)
        for p in ("docs/paf/schema/v0.1/common.schema.json",
                  "docs/paf/examples/full-agent/signature.json",
                  "pkg/claude/agent/skills/review/SKILL.md", "skills/x/SKILL.md",
                  "mkdocs.yaml", "docsx/a.md", "main.go", "docs/embed.go",
                  "docs/embed_darwin.go", "docs/learn/x.go"):
            self.assertFalse(cc.is_doc(p), p)

    def test_is_full(self):
        for p in (".github/workflows/ci.yml", ".github/actions/test-suite/action.yml",
                  ".github/copilot-smoke-scenarios.txt", ".github/CODEOWNERS",
                  "scripts/ci/classify-changes.py", "scripts/ci/pi/package-lock.json",
                  "scripts/ci/affected/selector.go", "go.mod", "go.sum", "mise.toml",
                  "scripts/test.sh"):
            self.assertTrue(cc.is_full(p), p)
        for p in ("scripts/testx.sh", "pkg/go.mod.go", "docs/go.sum", "scripts/cix/a"):
            self.assertFalse(cc.is_full(p), p)

    def test_is_known(self):
        for p in ("pkg/a/b.go", "cmd/x/main.go", "main.go", "scripts/lib/smoke/a.sh",
                  "skills/a/SKILL.md", ".claude/settings.json", ".golangci.yml",
                  "docs/paf/schema/x.json", "docs/x.md", "README.md", "LICENSE",
                  "docs/embed.go"):
            self.assertTrue(cc.is_known(p), p)
        for p in ("verify.sh", ".goreleaser.yaml", "Makefile", "vendor/x.go",
                  "internal/x.go", "foo/bar.txt", ".envrc"):
            self.assertFalse(cc.is_known(p), p)

    def test_is_ui(self):
        for p in ("pkg/claude/agentd/dashboard/board.css",
                  "pkg/claude/agentd/dashboard/js/app.js",
                  "pkg/claude/agentd/dashboard/dashboard.html",
                  "pkg/claude/agentd/dashboard/third_party/x/y.js",
                  "pkg/claude/agentd/jstest/foo.test.mjs",
                  "pkg/claude/agentd/jstest/ui-controls-baseline.json"):
            self.assertTrue(cc.is_ui(p), p)
        for p in ("pkg/claude/agentd/dashboard/assets.go",
                  "pkg/claude/agentd/dashboard/assets_test.go",
                  "pkg/claude/agentd/jstest/dashboard_node_test.go",
                  "pkg/claude/agentd/dashboard.go", "pkg/claude/agentd/dashboardx/a.css",
                  "docs/x.md"):
            self.assertFalse(cc.is_ui(p), p)

    def test_is_ui_go_test(self):
        for p in ("pkg/claude/agentd/dashboard_wizard_html_test.go",
                  "pkg/claude/agentd/dashboard_sandbox_profiles_html_test.go",
                  "pkg/claude/agentd/dashboard_capture_flow_test.go"):
            self.assertTrue(cc.is_ui_go_test(p, read), p)
        for p in ("pkg/claude/agentd/dashboard.go",
                  "pkg/claude/agentd/dashboard_config.go",
                  "pkg/claude/agentd/dashboard_x_darwin_test.go",
                  "pkg/claude/agentd/dashboard_x_linux_test.go",
                  "pkg/claude/agentd/dashboard_tagged_test.go",
                  "pkg/claude/agentd/sub/dashboard_x_test.go",
                  "pkg/claude/agentd/handlers_test.go",
                  "pkg/claude/agentd/dashboard/assets_test.go",
                  "pkg/claude/session/dashboard_x_test.go",
                  "pkg/claude/agentd/dashboard_x_test.go.orig"):
            self.assertFalse(cc.is_ui_go_test(p, read), p)
        self.assertFalse(cc.is_ui_go_test("pkg/claude/agentd/dashboard_gone_test.go",
                                          lambda p: None))

    def test_is_sandbox(self):
        for p in ("pkg/claude/session/launch.go", "pkg/claude/proxy/x.go",
                  "pkg/claude/sandboxproxy/x.go", "pkg/claude/common/sandboxpolicy/p.go",
                  "pkg/claude/sandboxassumptions/a_test.go", "pkg/claude/harness/codex.go",
                  "pkg/claude/harness/transcript/t.go", "pkg/claude/opencodeapi/c.go",
                  "pkg/claude/codexappserver/s.go", "pkg/harnesscredentials/c.go",
                  "pkg/harnessops/o.go", "pkg/common/harnesspath/p.go",
                  "scripts/filtered-proxy-smoke/run.sh", "scripts/proxy-posture-e2e/run.sh",
                  "scripts/seatbelt-smoke/driver.sh", "scripts/lib/smoke/common.sh",
                  "pkg/claude/agentd/opencode_runtime.go",
                  "pkg/claude/agentd/opencode_proxy_floor_smoke_linux_test.go",
                  "pkg/claude/agentd/sandbox_grants.go", "pkg/claude/agentd/netns.go",
                  "pkg/claude/agentd/Seatbelt.go", "pkg/x/bwrap.go", "pkg/x/passt.go",
                  "pkg/x/egress.go", "pkg/x/firewall.go", "pkg/x/network_policy.go",
                  "pkg/x/bubblewrap.go"):
            self.assertTrue(cc.is_sandbox(p), p)
        for p in ("pkg/claude/harness/pifixture/pi.go",
                  "pkg/claude/harness/copilotfixture/smoke_test.go",
                  "pkg/claude/harness/geminifixture/g.go",
                  "pkg/claude/harness/approvalfixture/a.go",
                  "pkg/claude/agentd/handlers.go", "pkg/claude/common/db/db.go",
                  "docs/sandbox.md", "scripts/e2e/proxy.sh",
                  "pkg/claude/agentd/dashboard/js/proxy-settings.js",
                  "pkg/claude/agentd/dashboard/sandbox.css",
                  "pkg/claude/agentd/jstest/network.test.mjs",
                  "pkg/claude/agentd/dashboard_sandbox_profiles_html_test.go",
                  # The keyword applies to the file name, not its directory.
                  "pkg/claude/proxyish/a.go"):
            self.assertFalse(cc.is_sandbox(p), p)

    def test_os_suffix(self):
        for p in ("a_darwin.go", "a_linux.go", "a_windows.go", "a_unix.go",
                  "a_freebsd.go", "a_darwin_test.go", "a_unix_test.go",
                  "a_amd64.go", "a_arm64_test.go", "a_darwin_arm64.go",
                  "a_linux_amd64_test.go", "dir/x_ios.go", "x_wasip1.go"):
            self.assertTrue(cc.has_os_suffix(p), p)
        for p in ("linux.go", "darwin_test.go", "a_test.go", "a.go", "a_darwinx.go",
                  "a_darwin.txt", "a_other_test.go", "a_mac.go"):
            self.assertFalse(cc.has_os_suffix(p), p)

    def test_go_source_is_platform(self):
        for text in ("//go:build darwin\n", "//go:build linux || darwin\n",
                     "//go:build !linux\n", "//go:build unix && !aix\n",
                     "//go:build !windows\n", "//go:build cgo\n",
                     "// +build linux\n", "package x\n\nimport \"C\"\n",
                     "package x\n\nimport (\n\t\"C\"\n\t\"os\"\n)\n",
                     "// doc\n//go:build integration && darwin\n",
                     'package x\n\nimport (\n\t"os"\n\t"C"\n)\n'):
            self.assertTrue(cc.go_source_is_platform(text), text)
        for text in ("package x\n", "//go:build integration\n", "//go:build ignore\n",
                     "// go:build darwin\n", "// mentions darwin in prose\n",
                     "//go:build linuxish\n", 'var s = "import \\"C\\""\n'):
            self.assertFalse(cc.go_source_is_platform(text), text)

    def test_is_platform_file(self):
        for p in ("pkg/claude/agentd/tray_darwin.go", "pkg/claude/agentd/tagged.go",
                  "pkg/claude/agentd/oldtag.go", "pkg/claude/agentd/cgo.go",
                  "pkg/claude/agentd/cgo_group.go", "pkg/claude/agentd/cgotag.go",
                  "pkg/claude/agentd/notunix.go", "pkg/x/native.c", "pkg/x/native.m",
                  "pkg/x/native.h", "pkg/x/asm.s",
                  "pkg/claude/harness/copilotfixture/testdata/1.0.77/sandbox_baseline_darwin.json",
                  "pkg/x/profile.sb", "pkg/x/macos_notes.txt", "pkg/x/Seatbelt.tmpl"):
            self.assertTrue(cc.is_platform_file(p, read), p)
        for p in ("pkg/claude/agentd/handlers.go", "pkg/claude/agentd/integration.go",
                  "pkg/x/a.js", "pkg/x/data.json", "pkg/x/README.md"):
            self.assertFalse(cc.is_platform_file(p, read), p)
        # Run-time OS branches count for the changed file itself...
        self.assertTrue(cc.is_platform_file("pkg/claude/setup/setup.go", read))
        self.assertTrue(cc.is_platform_file(
            "pkg/x/a.go", lambda p: "x := runtime.GOARCH\n"))
        self.assertFalse(cc.is_platform_file(
            "pkg/x/a.go", lambda p: "// runtimeXGOOS goosey\n"))
        # ...but do not make the whole package a platform package.
        self.assertFalse(cc.is_platform_file("pkg/claude/setup/setup.go", read,
                                             runtime_os=False))
        # Unreadable Go source is a doubt, and doubts run macOS.
        self.assertTrue(cc.is_platform_file("pkg/x/gone.go", lambda p: None))
        self.assertFalse(cc.is_platform_file("pkg/x/gone.txt", lambda p: None))

    def test_platform_dirs(self):
        dirs = cc.platform_dirs(GO_FILES, read)
        self.assertIn("pkg/hostmetrics", dirs)
        for d in cc.PER_FILE_ONLY_DIRS:
            self.assertNotIn(d, dirs)
        self.assertNotIn("pkg/claude/common/db", dirs)
        self.assertNotIn("pkg/claude/setup", dirs)


class Decisions(unittest.TestCase):
    def check_shape(self, mode, run, matrix):
        self.assertIn(mode, MODES)
        self.assertEqual(set(run), set(cc.JOBS))
        self.assertTrue(all(isinstance(v, bool) for v in run.values()))
        self.assertTrue(matrix, "the test matrix must never be empty")
        for entry in matrix:
            self.assertEqual(set(entry), {"os", "shard"})

    def decide(self, paths, affected=lambda pkg: False, reader=read):
        mode, run, matrix, reasons = classify(paths, affected, reader)
        self.check_shape(mode, run, matrix)
        self.assertTrue(reasons)
        return mode, run, matrix

    def assert_full(self, paths):
        mode, run, matrix = self.decide(paths, affected=never)
        self.assertEqual(mode, "full", paths)
        self.assertTrue(all(run.values()), paths)
        self.assertEqual(matrix, LINUX_ENTRIES + MAC_ENTRIES)

    def test_empty_diff_runs_everything(self):
        self.assert_full([])

    def test_ci_and_module_changes_run_everything(self):
        for p in (".github/workflows/ci.yml", ".github/workflows/release.yml",
                  ".github/actions/test-suite/action.yml",
                  ".github/copilot-smoke-scenarios.txt",
                  "scripts/ci/classify-changes.py", "scripts/ci/pi/package.json",
                  "scripts/ci/affected/main.go", "go.mod", "go.sum", "mise.toml",
                  "scripts/test.sh"):
            for extra in ([], ["docs/x.md"], ["pkg/claude/agentd/dashboard/board.css"],
                          ["pkg/claude/common/db/db.go"]):
                self.assert_full([p, *extra])

    def test_unknown_paths_run_everything(self):
        for p in ("verify.sh", ".goreleaser.yaml", "Makefile", "internal/a.go",
                  "vendor/m/a.go", "newdir/thing.txt"):
            self.assert_full([p])
            self.assert_full([p, "pkg/claude/agentd/dashboard/board.css"])

    def test_docs_only(self):
        for paths in (["docs/x.md"], ["README.md", "mkdocs.yml", "docs/img/a.png"],
                      ["CONTRIBUTING.md"]):
            mode, run, matrix = self.decide(paths, affected=never)
            self.assertEqual(mode, "docs")
            self.assertFalse(any(run.values()))
            # Placeholder only; the test job is skipped.
            self.assertEqual(matrix, [{"os": LINUX, "shard": "none"}])

    def test_ui_only(self):
        for paths in (["pkg/claude/agentd/dashboard/board.css"],
                      ["pkg/claude/agentd/dashboard/js/app.js", "docs/dashboard.md"],
                      ["pkg/claude/agentd/jstest/x.test.mjs",
                       "pkg/claude/agentd/dashboard/dashboard.html", "README.md"]):
            mode, run, matrix = self.decide(paths, affected=never)
            self.assertEqual(mode, "ui", paths)
            self.assertEqual({j for j, v in run.items() if v}, {"test"})
            self.assertEqual(matrix, [{"os": LINUX, "shard": s} for s in ("agentd", "jstest", "rest")])
            self.assertFalse(any(e["os"] == MACOS for e in matrix))

    def test_ui_with_dashboard_go_tests_adds_lint(self):
        paths = ["pkg/claude/agentd/dashboard/ui.css",
                 "pkg/claude/agentd/dashboard_styling_sweep_html_test.go",
                 "pkg/claude/agentd/dashboard_sandbox_profiles_html_test.go"]
        mode, run, matrix = self.decide(paths, affected=never)
        self.assertEqual(mode, "ui")
        self.assertEqual({j for j, v in run.items() if v}, {"test", "lint"})
        self.assertFalse(any(e["os"] == MACOS for e in matrix))
        # The Go test alone is ui as well.
        mode, run, _ = self.decide(paths[1:2], affected=never)
        self.assertEqual((mode, run["lint"]), ("ui", True))

    def test_go_beside_ui_is_code(self):
        for go in ("pkg/claude/agentd/dashboard/assets.go",
                   "pkg/claude/agentd/jstest/node_test.go",
                   "pkg/claude/agentd/handlers.go", "main.go",
                   "pkg/claude/agentd/dashboard.go",
                   "pkg/claude/agentd/dashboard_tagged_test.go",
                   "pkg/claude/agentd/dashboard_x_darwin_test.go",
                   "pkg/claude/agent/skills/review/SKILL.md",
                   "docs/paf/schema/v0.1/common.schema.json"):
            mode, run, matrix = self.decide(["pkg/claude/agentd/dashboard/board.css", go])
            self.assertEqual(mode, "code", go)
            self.assertTrue(run["lint"] and run["test"] and run["sandbox-v2-smoke"])
            self.assertEqual(matrix[:len(LINUX_ENTRIES)], LINUX_ENTRIES)

    def test_plain_code_skips_macos_and_proxy_smokes(self):
        mode, run, matrix = self.decide(["pkg/claude/agentd/handlers.go", "docs/x.md"])
        self.assertEqual(mode, "code")
        self.assertEqual(matrix, LINUX_ENTRIES)
        self.assertFalse(run["sandbox-v2-seatbelt-smoke"])
        self.assertFalse(run["filtered-proxy-smoke"])
        self.assertFalse(run["proxy-posture-e2e"])
        self.assertTrue(run["lint"] and run["test"] and run["sandbox-v2-smoke"])

    def assert_macos(self, path, want=True):
        mode, run, matrix = self.decide(["pkg/claude/agentd/handlers.go", path])
        self.assertEqual(mode, "code")
        has_mac = any(e["os"] == MACOS for e in matrix)
        self.assertEqual(has_mac, want, path)
        self.assertEqual(run["sandbox-v2-seatbelt-smoke"], want, path)
        if want:
            self.assertEqual(matrix, LINUX_ENTRIES + MAC_ENTRIES)

    def test_platform_files_run_macos(self):
        for p in ("pkg/claude/agentd/tray_darwin.go", "pkg/claude/agentd/x_linux.go",
                  "pkg/claude/agentd/x_unix_test.go", "pkg/claude/agentd/x_windows.go",
                  "pkg/claude/agentd/x_darwin_arm64.go", "pkg/claude/agentd/x_amd64.go",
                  "pkg/claude/agentd/tagged.go", "pkg/claude/agentd/oldtag.go",
                  "pkg/claude/agentd/cgo.go", "pkg/claude/agentd/cgotag.go",
                  "pkg/claude/agentd/notunix.go",
                  "pkg/claude/agentd/jstest/suite_memory_other_test.go",
                  "pkg/claude/harness/opencode_sandbox_darwin_test.go",
                  "pkg/x/native.m", "pkg/x/profile.sb",
                  "pkg/claude/harness/copilotfixture/testdata/1.0.77/sandbox_baseline_darwin.json"):
            self.assert_macos(p)

    def test_runtime_os_branch_runs_macos_for_that_file_only(self):
        self.assert_macos("pkg/claude/setup/setup.go")
        self.assert_macos("pkg/claude/setup/other.go", want=False)

    def test_go_under_docs_is_code(self):
        for p in ("docs/embed.go", "docs/embed_test.go"):
            mode, run, _ = self.decide([p, "docs/x.md"])
            self.assertEqual(mode, "code", p)
            self.assertTrue(run["lint"] and run["test"], p)
        self.assert_macos("docs/embed_darwin.go")

    def test_platform_package_runs_macos_for_any_file(self):
        self.assert_macos("pkg/hostmetrics/read.go")
        self.assert_macos("pkg/hostmetrics/testdata/sample.txt")
        self.assert_macos("pkg/hostmetrics/new_file.go")

    def test_per_file_only_packages(self):
        for p in ("pkg/claude/agentd/handlers.go", "pkg/claude/agentd/integration.go",
                  "pkg/claude/agentd/jstest/node_test.go",
                  "pkg/claude/harness/pifixture/pi.go", "pkg/claude/common/db/db.go"):
            self.assert_macos(p, want=False)

    def test_sandbox_paths_run_macos_and_proxy_smokes(self):
        for p in ("pkg/claude/session/launch.go", "pkg/claude/harness/harness.go",
                  "pkg/claude/agentd/opencode_runtime.go", "scripts/lib/smoke/common.sh",
                  "scripts/filtered-proxy-smoke/flows/10-floor-policy.sh",
                  "scripts/proxy-posture-e2e/run.sh", "scripts/seatbelt-smoke/driver.sh",
                  "pkg/claude/common/sandboxpolicy/policy.go"):
            mode, run, matrix = self.decide([p])
            self.assertEqual(mode, "code", p)
            self.assertTrue(run["filtered-proxy-smoke"] and run["proxy-posture-e2e"], p)
            self.assertTrue(run["sandbox-v2-seatbelt-smoke"], p)
            self.assertEqual(matrix, LINUX_ENTRIES + MAC_ENTRIES)

    def test_harness_smokes_follow_affected(self):
        for reached in itertools.chain.from_iterable(
                itertools.combinations(cc.HARNESS_SMOKES.values(), n) for n in range(4)):
            asked = []

            def affected(pkg):
                asked.append(pkg)
                return pkg in reached
            mode, run, _ = self.decide(["pkg/claude/common/db/db.go"], affected=affected)
            self.assertEqual(sorted(asked), sorted(cc.HARNESS_SMOKES.values()))
            for job, pkg in cc.HARNESS_SMOKES.items():
                self.assertEqual(run[job], pkg in reached, (job, reached))

    def test_harness_smokes_never_skip_in_full(self):
        mode, run, _ = self.decide(["go.mod"], affected=never)
        for job in cc.HARNESS_SMOKES:
            self.assertTrue(run[job])


RANK = {m: i for i, m in enumerate(MODES)}
POOL = [
    "docs/x.md", "README.md", "mkdocs.yml",
    "pkg/claude/agentd/dashboard/board.css", "pkg/claude/agentd/dashboard/js/a.js",
    "pkg/claude/agentd/jstest/x.test.mjs", "pkg/claude/agentd/dashboard/assets.go",
    "pkg/claude/agentd/dashboard_wizard_html_test.go",
    "pkg/claude/agentd/dashboard_tagged_test.go",
    "pkg/claude/agentd/jstest/node_test.go", "pkg/claude/agentd/handlers.go",
    "pkg/claude/agentd/tray_darwin.go", "pkg/claude/agentd/tagged.go",
    "pkg/hostmetrics/read.go", "pkg/claude/session/launch.go",
    "pkg/claude/setup/setup.go", "pkg/claude/setup/other.go", "docs/embed.go",
    "pkg/claude/common/db/db.go", "pkg/claude/harness/pifixture/pi.go",
    "scripts/lib/smoke/common.sh", "docs/paf/schema/x.json",
    ".github/workflows/ci.yml", "go.sum", "verify.sh",
]


def jobs_run(run, matrix):
    out = {j for j, v in run.items() if v and j != "test"}
    if run["test"]:
        out |= {(e["os"], e["shard"]) for e in matrix}
    return out


class Monotonic(unittest.TestCase):
    """Adding a changed path can only ever add CI, never remove it."""

    def test_superset_never_runs_less(self):
        rng = random.Random(1234)

        def affected_for(paths):
            # Model the import graph: db reaches every fixture, the pi fixture
            # reaches only itself.
            def affected(pkg):
                return ("pkg/claude/common/db/db.go" in paths
                        or ("pkg/claude/harness/pifixture/pi.go" in paths and "pifixture" in pkg))
            return affected

        for _ in range(3000):
            small = rng.sample(POOL, rng.randint(1, 4))
            big = small + rng.sample(POOL, rng.randint(1, 4))
            m1, r1, x1, _ = classify(small, affected_for(small))
            m2, r2, x2, _ = classify(big, affected_for(big))
            self.assertGreaterEqual(RANK[m2], RANK[m1], (small, big))
            self.assertLessEqual(jobs_run(r1, x1), jobs_run(r2, x2), (small, big))

    def test_exhaustive_pairs(self):
        for a, b in itertools.product(POOL, repeat=2):
            m1, r1, x1, _ = classify([a], lambda p: True)
            m2, r2, x2, _ = classify([a, b], lambda p: True)
            self.assertGreaterEqual(RANK[m2], RANK[m1], (a, b))
            self.assertLessEqual(jobs_run(r1, x1), jobs_run(r2, x2), (a, b))


class GitPlumbing(unittest.TestCase):
    """The real script against a throwaway repository."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.tmp.name)
        self.git("init", "--quiet")
        self.git("config", "user.email", "fixture@example.invalid")
        self.git("config", "user.name", "Fixture")
        self.write("main.go", "package main\n")
        self.write("pkg/claude/agentd/plain.go", "package agentd\n")
        self.write("pkg/claude/agentd/tagged.go", "//go:build darwin\n\npackage agentd\n")
        self.write("pkg/hostmetrics/read.go", "package hostmetrics\n")
        self.write("pkg/hostmetrics/read_darwin.go", "package hostmetrics\n")
        self.commit("base")
        self.base = self.rev()

    def tearDown(self):
        self.tmp.cleanup()

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.root)

    def write(self, name, text):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)
        self.git("add", "--", name)

    def commit(self, msg):
        self.git("commit", "--quiet", "--allow-empty", "-m", msg)

    def rev(self):
        return self.git("rev-parse", "HEAD").decode().strip()

    def run_script(self, *args, no_go=False):
        env = dict(os.environ)
        if no_go:
            # A PATH holding git and nothing else: no Go toolchain.
            bin_dir = self.root / ".git" / "fixture-bin"
            bin_dir.mkdir(exist_ok=True)
            link = bin_dir / "git"
            if not link.exists():
                link.symlink_to(shutil.which("git"))
            env["PATH"] = str(bin_dir)
        return subprocess.run([sys.executable, str(SCRIPT), *args], cwd=self.root,
                              capture_output=True, text=True, env=env)

    def outputs(self, *args, **kw):
        res = self.run_script(*args, **kw)
        self.assertEqual(res.returncode, 0, res.stderr)
        out = dict(line.split("=", 1) for line in res.stdout.splitlines())
        expected = json.loads(out["expected"])
        self.assertEqual(set(expected), set(cc.JOBS))
        for job in cc.JOBS:
            self.assertEqual(out[f"run_{job.replace('-', '_')}"],
                             "true" if expected[job] else "false")
        out["matrix"] = json.loads(out["test_matrix"])["include"]
        return out

    def test_empty_diff_is_full(self):
        self.assertEqual(self.outputs(self.base)["mode"], "full")

    def test_docs_and_ui(self):
        self.write("docs/a name\nwith newline.md", "x\n")
        self.write("README.md", "x\n")
        self.commit("docs")
        self.assertEqual(self.outputs(self.base)["mode"], "docs")
        self.write("pkg/claude/agentd/dashboard/board.css", "a{}\n")
        self.commit("ui")
        out = self.outputs(self.base)
        self.assertEqual(out["mode"], "ui")
        # The explicit head argument agrees with the default HEAD.
        self.assertEqual(self.outputs(self.base, "HEAD")["mode"], "ui")

    def test_code_rename_into_docs_is_code(self):
        self.git("mv", "pkg/claude/agentd/plain.go", "README.md")
        self.commit("rename")
        # No Go toolchain on PATH: the harness verdicts must fail closed.
        out = self.outputs(self.base, no_go=True)
        self.assertEqual(out["mode"], "code")
        for job in cc.HARNESS_SMOKES:
            self.assertEqual(out[f"run_{job.replace('-', '_')}"], "true")

    def test_affected_failure_fails_closed(self):
        self.write("pkg/claude/agentd/plain.go", "package agentd // edited\n")
        self.commit("edit")
        # `go` exists but scripts/ci/affected does not in this repo: go run fails.
        out = self.outputs(self.base)
        self.assertEqual(out["mode"], "code")
        for job in cc.HARNESS_SMOKES:
            self.assertEqual(out[f"run_{job.replace('-', '_')}"], "true")
        self.assertFalse(any(e["os"] == MACOS for e in out["matrix"]))

    def test_deleted_build_tagged_file_is_read_from_base(self):
        self.git("rm", "--quiet", "pkg/claude/agentd/tagged.go")
        self.commit("delete")
        out = self.outputs(self.base, no_go=True)
        self.assertEqual(out["mode"], "code")
        self.assertTrue(any(e["os"] == MACOS for e in out["matrix"]))

    def test_removed_build_constraint_is_platform(self):
        self.write("pkg/claude/agentd/tagged.go", "package agentd\n")
        self.commit("drop the constraint")
        out = self.outputs(self.base, no_go=True)
        self.assertTrue(any(e["os"] == MACOS for e in out["matrix"]))

    def test_added_build_constraint_is_platform(self):
        self.write("pkg/claude/agentd/plain.go", "//go:build linux\n\npackage agentd\n")
        self.commit("add a constraint")
        out = self.outputs(self.base, no_go=True)
        self.assertTrue(any(e["os"] == MACOS for e in out["matrix"]))

    def test_plain_agentd_edit_skips_macos(self):
        self.write("pkg/claude/agentd/plain.go", "package agentd // edited\n")
        self.commit("edit")
        out = self.outputs(self.base, no_go=True)
        self.assertFalse(any(e["os"] == MACOS for e in out["matrix"]))

    def test_platform_package_from_head_tree(self):
        self.write("pkg/hostmetrics/read.go", "package hostmetrics // edited\n")
        self.commit("edit")
        out = self.outputs(self.base, no_go=True)
        self.assertTrue(any(e["os"] == MACOS for e in out["matrix"]))

    def test_new_platform_package_counts(self):
        self.write("pkg/newplat/a.go", "package newplat\n")
        self.write("pkg/newplat/a_darwin.go", "package newplat\n")
        self.commit("new")
        base = self.rev()
        self.write("pkg/newplat/a.go", "package newplat // edited\n")
        self.commit("edit")
        out = self.outputs(base, no_go=True)
        self.assertTrue(any(e["os"] == MACOS for e in out["matrix"]))

    def test_missing_base_fails(self):
        for args in (("missing-base",), (self.base, "missing-head"), ()):
            res = self.run_script(*args)
            self.assertNotEqual(res.returncode, 0, args)
            self.assertNotIn("mode=", res.stdout)


class WorkflowConsistency(unittest.TestCase):
    """Pin the classifier's job list to what ci.yml actually wires."""

    @classmethod
    def setUpClass(cls):
        cls.ci = (ROOT / ".github/workflows/ci.yml").read_text()
        cls.action = (ROOT / ".github/actions/test-suite/action.yml").read_text()

    def job_block(self, name):
        m = re.search(rf"^  {re.escape(name)}:\n(.*?)(?=^  [a-z][\w-]*:\n|\Z)",
                      self.ci, re.MULTILINE | re.DOTALL)
        self.assertIsNotNone(m, name)
        return m.group(1)

    def test_ci_complete_needs_every_job(self):
        section = self.ci[self.ci.index("\njobs:\n"):]
        jobs = set(re.findall(r"^  ([a-z][\w-]*):\n", section, re.MULTILINE))
        needs = re.search(r"^    needs: \[(.*)\]$", self.job_block("ci-complete"), re.MULTILINE)
        needs = {n.strip() for n in needs.group(1).split(",")}
        self.assertEqual(needs, jobs - {"ci-complete"})
        self.assertEqual(needs - {"changes", "docs", "learn-commands"}, set(cc.JOBS))

    def test_each_gated_job_uses_its_own_verdict(self):
        outputs = self.job_block("changes")
        for job in cc.JOBS:
            key = "run_" + job.replace("-", "_")
            block = self.job_block(job)
            self.assertIn("    needs: changes\n", block, job)
            self.assertIn(f"    if: needs.changes.outputs.{key} == 'true'\n", block, job)
            self.assertIn(f"      {key}: ${{{{ steps.diff.outputs.{key} }}}}\n", outputs, job)
        for key in ("mode", "expected", "test_matrix"):
            self.assertIn(f"      {key}: ${{{{ steps.diff.outputs.{key} }}}}\n", outputs, key)

    def test_always_on_jobs_are_ungated(self):
        for job in ("changes", "docs", "learn-commands"):
            self.assertNotIn("\n    if:", "\n" + self.job_block(job), job)

    def test_test_matrix_comes_from_classifier(self):
        block = self.job_block("test")
        self.assertIn("matrix: ${{ fromJSON(needs.changes.outputs.test_matrix) }}", block)
        self.assertIn("diff-base: HEAD^1", block)

    def test_shards_exist_in_test_suite_action(self):
        for shard in set(cc.LINUX_SHARDS) | set(cc.UI_SHARDS) | set(cc.MACOS_SHARDS):
            self.assertRegex(self.action, rf"\n\s+{re.escape(shard)}\)", shard)


class TreeConsistency(unittest.TestCase):
    """The path rules name real places in this repository."""

    def test_named_dirs_exist(self):
        for d in cc.PER_FILE_ONLY_DIRS:
            self.assertTrue((ROOT / d).is_dir(), d)
        for prefix in cc.SANDBOX_PREFIXES + cc.UI_PREFIXES:
            self.assertTrue((ROOT / prefix).is_dir(), prefix)
        for pkg in cc.HARNESS_SMOKES.values():
            self.assertTrue((ROOT / pkg).is_dir(), pkg)

    def test_every_tracked_root_is_classified(self):
        # Not required for safety (unknown paths run everything), but a new
        # top-level file silently forcing full CI on every PR that touches it
        # should be a decision, not an accident.
        try:
            files = subprocess.check_output(["git", "ls-files", "-z"], cwd=ROOT).split(b"\0")
        except (OSError, subprocess.CalledProcessError):
            self.skipTest("not a git checkout")
        allowed_full = {".goreleaser.yaml", "verify.sh"}
        for f in (p.decode() for p in files if p):
            if cc.is_full(f) or f in allowed_full:
                continue
            self.assertTrue(cc.is_known(f), f)


if __name__ == "__main__":
    unittest.main()
