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
LINUX_ENTRIES = [{"os": LINUX, "shard": s} for s in cc.LINUX_SHARDS]
MAC_ENTRIES = [{"os": MACOS, "shard": s} for s in cc.MACOS_SHARDS]

def never(pkg):
    raise AssertionError(f"affected({pkg}) must not be consulted here")


def classify(paths, affected=lambda pkg: False, macos=False):
    return cc.classify(paths, affected, macos=macos)


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
                  "pkg/claude/agentd/dashboard_capture_flow_test.go",
                  # PR CI is Linux-only, so a platform suffix changes nothing.
                  "pkg/claude/agentd/dashboard_x_darwin_test.go",
                  "pkg/claude/agentd/dashboard_x_linux_test.go"):
            self.assertTrue(cc.is_ui_go_test(p), p)
        for p in ("pkg/claude/agentd/dashboard.go",
                  "pkg/claude/agentd/dashboard_config.go",
                  "pkg/claude/agentd/sub/dashboard_x_test.go",
                  "pkg/claude/agentd/handlers_test.go",
                  "pkg/claude/agentd/dashboard/assets_test.go",
                  "pkg/claude/session/dashboard_x_test.go",
                  "pkg/claude/agentd/dashboard_x_test.go.orig"):
            self.assertFalse(cc.is_ui_go_test(p), p)

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

class Decisions(unittest.TestCase):
    def check_shape(self, mode, run, matrix, macos=False):
        self.assertIn(mode, MODES)
        self.assertEqual(set(run), set(cc.JOBS))
        self.assertTrue(all(isinstance(v, bool) for v in run.values()))
        self.assertTrue(matrix, "the test matrix must never be empty")
        for entry in matrix:
            self.assertEqual(set(entry), {"os", "shard"})
        # Without the opt-in, PR CI runs no macOS jobs (CONTRIBUTING.md,
        # "macOS CI"); with it, macOS runs exactly when the tests do.
        has_mac = any(e["os"] == MACOS for e in matrix)
        self.assertEqual(has_mac, macos and run["test"], (mode, matrix))
        self.assertEqual(run["sandbox-v2-seatbelt-smoke"], has_mac, (mode, run))
        if has_mac:
            self.assertEqual(matrix[-len(MAC_ENTRIES):], MAC_ENTRIES)

    def decide(self, paths, affected=lambda pkg: False, macos=False):
        mode, run, matrix, reasons = classify(paths, affected, macos)
        self.check_shape(mode, run, matrix, macos)
        self.assertTrue(reasons)
        return mode, run, matrix

    def assert_full(self, paths):
        mode, run, matrix = self.decide(paths, affected=never)
        self.assertEqual(mode, "full", paths)
        # Everything but the macOS opt-in job, which needs the opt-in.
        self.assertTrue(all(v for j, v in run.items() if j not in cc.OPT_IN_JOBS), paths)
        self.assertEqual(matrix, LINUX_ENTRIES)

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

    def test_ui_with_dashboard_go_tests_adds_lint(self):
        paths = ["pkg/claude/agentd/dashboard/ui.css",
                 "pkg/claude/agentd/dashboard_styling_sweep_html_test.go",
                 "pkg/claude/agentd/dashboard_sandbox_profiles_html_test.go"]
        mode, run, matrix = self.decide(paths, affected=never)
        self.assertEqual(mode, "ui")
        self.assertEqual({j for j, v in run.items() if v}, {"test", "lint"})
        # The Go test alone is ui as well.
        mode, run, _ = self.decide(paths[1:2], affected=never)
        self.assertEqual((mode, run["lint"]), ("ui", True))

    def test_go_beside_ui_is_code(self):
        for go in ("pkg/claude/agentd/dashboard/assets.go",
                   "pkg/claude/agentd/jstest/node_test.go",
                   "pkg/claude/agentd/handlers.go", "main.go",
                   "pkg/claude/agentd/dashboard.go",
                   "pkg/claude/agent/skills/review/SKILL.md",
                   "docs/paf/schema/v0.1/common.schema.json"):
            mode, run, matrix = self.decide(["pkg/claude/agentd/dashboard/board.css", go])
            self.assertEqual(mode, "code", go)
            self.assertTrue(run["lint"] and run["test"] and run["sandbox-v2-smoke"])
            self.assertEqual(matrix, LINUX_ENTRIES)

    def test_plain_code_skips_proxy_smokes(self):
        mode, run, matrix = self.decide(["pkg/claude/agentd/handlers.go", "docs/x.md"])
        self.assertEqual(mode, "code")
        self.assertEqual(matrix, LINUX_ENTRIES)
        self.assertFalse(run["filtered-proxy-smoke"])
        self.assertFalse(run["proxy-posture-e2e"])
        self.assertTrue(run["lint"] and run["test"] and run["sandbox-v2-smoke"])

    def test_platform_code_runs_on_linux_only(self):
        # These used to add the macOS jobs; release.yml now measures darwin.
        for p in ("pkg/claude/agentd/tray_darwin.go", "pkg/claude/agentd/x_unix_test.go",
                  "pkg/claude/agentd/x_darwin_arm64.go", "pkg/x/native.m",
                  "pkg/x/profile.sb", "docs/embed_darwin.go",
                  "pkg/claude/harness/opencode_sandbox_darwin_test.go",
                  "pkg/claude/harness/copilotfixture/testdata/1.0.77/sandbox_baseline_darwin.json"):
            mode, run, matrix = self.decide(["pkg/claude/agentd/handlers.go", p])
            self.assertEqual(mode, "code", p)
            self.assertEqual(matrix, LINUX_ENTRIES, p)

    def test_go_under_docs_is_code(self):
        for p in ("docs/embed.go", "docs/embed_test.go", "docs/embed_darwin.go"):
            mode, run, _ = self.decide([p, "docs/x.md"])
            self.assertEqual(mode, "code", p)
            self.assertTrue(run["lint"] and run["test"], p)

    def test_sandbox_paths_run_proxy_smokes(self):
        for p in ("pkg/claude/session/launch.go", "pkg/claude/harness/harness.go",
                  "pkg/claude/agentd/opencode_runtime.go", "scripts/lib/smoke/common.sh",
                  "scripts/filtered-proxy-smoke/flows/10-floor-policy.sh",
                  "scripts/proxy-posture-e2e/run.sh", "scripts/seatbelt-smoke/driver.sh",
                  "pkg/claude/common/sandboxpolicy/policy.go"):
            mode, run, matrix = self.decide([p])
            self.assertEqual(mode, "code", p)
            self.assertTrue(run["filtered-proxy-smoke"] and run["proxy-posture-e2e"], p)
            self.assertEqual(matrix, LINUX_ENTRIES)

    def test_macos_opt_in_adds_macos_wherever_tests_run(self):
        cases = (
            (["docs/x.md"], "docs", None),
            (["pkg/claude/agentd/dashboard/board.css"], "ui",
             [{"os": LINUX, "shard": s} for s in cc.UI_SHARDS]),
            (["pkg/claude/agentd/handlers.go"], "code", LINUX_ENTRIES),
            (["go.mod"], "full", LINUX_ENTRIES),
        )
        for paths, want_mode, linux in cases:
            mode, run, matrix = self.decide(paths, affected=lambda p: True, macos=True)
            self.assertEqual(mode, want_mode, paths)
            if linux is None:
                self.assertFalse(run["test"] or run["sandbox-v2-seatbelt-smoke"], paths)
                continue
            self.assertEqual(matrix, linux + MAC_ENTRIES, paths)
            self.assertTrue(run["sandbox-v2-seatbelt-smoke"], paths)
            # The opt-in only adds macOS; every Linux verdict is unchanged.
            _, base_run, base_matrix = self.decide(paths, affected=lambda p: True)
            self.assertEqual(base_matrix, linux, paths)
            self.assertEqual({j: v for j, v in run.items() if j != "sandbox-v2-seatbelt-smoke"},
                             {j: v for j, v in base_run.items() if j != "sandbox-v2-seatbelt-smoke"})
            self.assertFalse(base_run["sandbox-v2-seatbelt-smoke"], paths)

    def test_macos_opt_in_sources(self):
        self.assertTrue(cc.macos_opt_in(["ci:macos"], []))
        self.assertTrue(cc.macos_opt_in(["bug", "ci:macos"], []))
        self.assertTrue(cc.macos_opt_in([], ["true"]))
        self.assertTrue(cc.macos_opt_in([], [" TRUE "]))
        self.assertTrue(cc.macos_opt_in(["ci:macos"], ["true"]))
        self.assertFalse(cc.macos_opt_in([], []))
        self.assertFalse(cc.macos_opt_in(["ci:mac", "macos", "CI:MACOS"], []))
        self.assertFalse(cc.macos_opt_in([], ["false", "yes", ""]))

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

        for i in range(3000):
            small = rng.sample(POOL, rng.randint(1, 4))
            big = small + rng.sample(POOL, rng.randint(1, 4))
            macos = bool(i % 2)
            m1, r1, x1, _ = classify(small, affected_for(small), macos)
            m2, r2, x2, _ = classify(big, affected_for(big), macos)
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

    def test_platform_edits_stay_on_linux(self):
        self.write("pkg/claude/agentd/tagged.go", "package agentd\n")
        self.write("pkg/claude/agentd/plain.go", "//go:build linux\n\npackage agentd\n")
        self.commit("move a build constraint")
        out = self.outputs(self.base, no_go=True)
        self.assertEqual(out["mode"], "code")
        self.assertEqual(out["matrix"], LINUX_ENTRIES)

    def commit_message(self, msg):
        self.git("commit", "--quiet", "--allow-empty", "-m", msg)

    def assert_macos(self, out, want):
        has_mac = any(e["os"] == MACOS for e in out["matrix"])
        self.assertEqual(has_mac, want, out["matrix"])
        self.assertEqual(out["run_sandbox_v2_seatbelt_smoke"], "true" if want else "false")

    def test_macos_off_by_default(self):
        self.write("pkg/claude/agentd/plain.go", "package agentd // edited\n")
        self.commit("edit")
        head = self.rev()
        self.assert_macos(self.outputs(self.base, no_go=True), False)
        self.assert_macos(self.outputs(self.base, "--pr-head", head, "--labels", "bug,docs",
                                       no_go=True), False)

    def test_macos_label_opts_in(self):
        self.write("pkg/claude/agentd/plain.go", "package agentd // edited\n")
        self.commit("edit")
        for labels in ("ci:macos", "bug,ci:macos", "bug\nci:macos\n"):
            out = self.outputs(self.base, "--labels", labels, "--pr-head", self.rev(), no_go=True)
            self.assert_macos(out, True)

    def test_macos_trailer_on_head_commit_opts_in(self):
        self.write("pkg/claude/agentd/plain.go", "package agentd // edited\n")
        self.commit_message("edit\n\nCI-macOS: true\nCo-Authored-By: A <a@example.invalid>")
        self.assert_macos(self.outputs(self.base, "--pr-head", self.rev(), no_go=True), True)
        # The trailer is read from --pr-head only, never from <head> itself.
        self.assert_macos(self.outputs(self.base, no_go=True), False)

    def test_macos_trailer_on_earlier_commit_is_ignored(self):
        self.write("pkg/claude/agentd/plain.go", "package agentd // edited\n")
        self.commit_message("edit\n\nCI-macOS: true")
        self.write("pkg/claude/agentd/plain.go", "package agentd // edited again\n")
        self.commit_message("follow-up without the trailer")
        self.assert_macos(self.outputs(self.base, "--pr-head", self.rev(), no_go=True), False)

    def test_macos_trailer_must_be_a_trailer(self):
        self.write("pkg/claude/agentd/plain.go", "package agentd // edited\n")
        self.commit_message("edit\n\nMentions CI-macOS: true in prose, then more prose.")
        self.assert_macos(self.outputs(self.base, "--pr-head", self.rev(), no_go=True), False)
        self.commit_message("edit\n\nCI-macOS: false")
        self.assert_macos(self.outputs(self.base, "--pr-head", self.rev(), no_go=True), False)

    def test_self_hosted_macos_only_with_the_flag(self):
        # The flag picks the runner; it never opts in by itself.
        self.write("pkg/claude/agentd/plain.go", "package agentd // edited\n")
        self.commit_message("edit\n\nCI-macOS: true")
        head = self.rev()
        hosted = self.outputs(self.base, "--pr-head", head, no_go=True)
        self.assert_macos(hosted, True)
        self.assertEqual(json.loads(hosted["macos_runner"]), MACOS)
        own = self.outputs(self.base, "--pr-head", head, "--self-hosted-macos", no_go=True)
        self.assertEqual(json.loads(own["macos_runner"]), cc.MACOS_SELF_HOSTED)
        mac = [e for e in own["matrix"] if e["os"] != LINUX]
        self.assertEqual(mac, [{"os": cc.MACOS_SELF_HOSTED, "shard": s} for s in cc.MACOS_SHARDS])
        self.assertEqual(own["run_sandbox_v2_seatbelt_smoke"], "true")
        self.commit_message("follow-up without the trailer")
        off = self.outputs(self.base, "--pr-head", self.rev(), "--self-hosted-macos", no_go=True)
        self.assertEqual([e for e in off["matrix"] if e["os"] != LINUX], [])
        self.assertEqual(off["run_sandbox_v2_seatbelt_smoke"], "false")

    def test_missing_pr_head_fails(self):
        res = self.run_script(self.base, "--pr-head", "0" * 40)
        self.assertNotEqual(res.returncode, 0)
        self.assertNotIn("mode=", res.stdout)

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

    def test_macos_only_behind_the_opt_in(self):
        # The one macOS runner outside the matrix is the opt-in-gated Seatbelt
        # smoke, which takes it from the classifier like the test job's macOS
        # entries do. No job names a macOS or self-hosted runner literally.
        self.assertEqual(re.findall(r"(?m)^\s*runs-on:\s*\[?\s*(?:macos|self-hosted)", self.ci), [])
        self.assertIn("    runs-on: ${{ fromJSON(needs.changes.outputs.macos_runner) }}\n",
                      self.job_block("sandbox-v2-seatbelt-smoke"))
        self.assertIn("      macos_runner: ${{ steps.diff.outputs.macos_runner }}\n",
                      self.job_block("changes"))
        self.assertIn("    runs-on: ${{ matrix.os }}\n", self.job_block("test"))

    def test_fork_prs_never_reach_the_self_hosted_runner(self):
        # The opt-in trailer is fork-controlled, so the self-hosted route is
        # granted by the head repository alone, in the workflow, not by
        # anything the PR's commits say.
        block = self.job_block("changes")
        self.assertIn("HEAD_REPO: ${{ github.event.pull_request.head.repo.full_name }}", block)
        guard = ('if [ -n "$HEAD_REPO" ] && [ "$HEAD_REPO" = "$GITHUB_REPOSITORY" ]; then\n'
                 '            runner_args=(--self-hosted-macos)\n'
                 '          fi\n')
        self.assertIn(guard, block)
        self.assertEqual(self.ci.count("--self-hosted-macos"), 1)
        self.assertIn('"${runner_args[@]}"', block)
        # Hosted macOS is the default: without the flag the classifier never
        # names the self-hosted runner.
        self.assertEqual(cc.macos_runner(False), "macos-latest")
        self.assertEqual(cc.macos_runner(True), ["self-hosted", "macOS", "ARM64"])
        # main keeps the hosted runner.
        for name in ("release.yml", "manual-release.yml"):
            text = (ROOT / ".github/workflows" / name).read_text()
            self.assertEqual(re.findall(r"(?m)^\s*runs-on:.*self-hosted", text), [], name)
            self.assertRegex(text, r"\n    runs-on: macos-latest\n", name)

    def test_gate_exempts_only_the_opt_in_jobs(self):
        m = re.search(r"opt_in = \{(.*)\}", self.job_block("ci-complete"))
        self.assertIsNotNone(m)
        names = {n.strip().strip('"') for n in m.group(1).split(",")}
        self.assertEqual(names, set(cc.OPT_IN_JOBS))

    def test_classifier_gets_the_opt_in_inputs(self):
        block = self.job_block("changes")
        self.assertIn("HEAD_SHA: ${{ github.event.pull_request.head.sha }}", block)
        self.assertIn('--labels "$labels" --pr-head "$HEAD_SHA"', block)
        self.assertIn("pull-requests: read", block)
        self.assertIn(cc.MACOS_LABEL, block)


class TreeConsistency(unittest.TestCase):
    """The path rules name real places in this repository."""

    def test_named_dirs_exist(self):
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
