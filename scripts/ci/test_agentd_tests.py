"""The runner must cover every side concurrently and retain readable failures."""
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
FAKE_GO = r'''#!/usr/bin/env python3
import json, os, re, sys, time
from pathlib import Path
SIDES = 3
args = sys.argv
root = Path(os.environ["FAKE_GO_STATE"])
if "-json" not in args:
    # The serial prebuild: it must precede both test processes.
    if args[args.index("-run")+1] != "^$" or "-skip" in args:
        print("prebuild must select no tests", file=sys.stderr)
        sys.exit(1)
    if os.getenv("FAKE_GO_BUILD_FAIL"):
        print("compile error details")
        sys.exit(1)
    (root / "built").touch()
    sys.exit(0)
if not (root / "built").exists():
    print("test process started before the prebuild", file=sys.stderr)
    sys.exit(1)
run = args[args.index("-run")+1] if "-run" in args else None
skip = args[args.index("-skip")+1] if "-skip" in args else None
side = str(os.getpid())
(root / (side+".started")).touch()
deadline = time.monotonic()+2
while len(list(root.glob("*.started"))) < SIDES:
    if time.monotonic() > deadline:
        print("test processes did not run concurrently", file=sys.stderr)
        sys.exit(1)
    time.sleep(.005)
names = ["TestDashboardNewFeature", "TestFederationMail", "TestLifecycle",
         "TestFutureFeature", "TestSpawnNewHarness", "TestPiDrive", "TestRealGitFetch",
         "TestCopilotDrive", "TestOpenCodeHook", "ExampleCLI", "FuzzParser"]
selected = [name for name in names
            if (run is None or re.match(run, name)) and not (skip and re.match(skip, name))]
(root / (side+".names")).write_text("\n".join(selected))
failed = False
for name in selected:
    if name == os.getenv("FAKE_GO_FAIL"):
        print(json.dumps({"Action":"output","Test":name,"Output":"assertion details\n"}))
        print(json.dumps({"Action":"fail","Test":name}))
        failed = True
    else:
        print(json.dumps({"Action":"pass","Test":name}))
print(json.dumps({"Action":"fail" if failed else "pass","Elapsed":.01}))
sys.exit(1 if failed else 0)
'''


class RunnerTests(unittest.TestCase):
    def run_fixture(self, failing="", build_fail=""):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            (root / "go").write_text(FAKE_GO)
            (root / "go").chmod(0o755)
            env = dict(os.environ, PATH=str(root)+os.pathsep+os.environ["PATH"],
                       FAKE_GO_STATE=str(root), FAKE_GO_FAIL=failing,
                       FAKE_GO_BUILD_FAIL=build_fail,
                       TCLAUDE_AGENTD_TEST_LOG_DIR=str(root / "logs"))
            result = subprocess.run(["bash", "scripts/ci/agentd-tests.sh", "-count=1"],
                                    cwd=ROOT, env=env, text=True, capture_output=True,
                                    timeout=10)
            if build_fail:
                return result, None
            sides = [set(f.read_text().splitlines()) for f in root.glob("*.names")]
            self.assertEqual(len(sides), 3, result.stdout+result.stderr)
            return result, sides

    def test_concurrent_exact_complement_includes_future_tests_and_examples(self):
        result, sides = self.run_fixture()
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        everything = set()
        for side in sides:
            self.assertFalse(everything & side, "sides overlap")
            everything |= side
        self.assertEqual(everything, {"TestDashboardNewFeature", "TestFederationMail",
                         "TestLifecycle", "TestFutureFeature", "TestSpawnNewHarness", "TestPiDrive",
                         "TestRealGitFetch", "TestCopilotDrive", "TestOpenCodeHook", "ExampleCLI", "FuzzParser"})
        self.assertIn({"TestFederationMail", "TestPiDrive", "TestOpenCodeHook"}, sides)
        self.assertIn({"TestDashboardNewFeature", "TestSpawnNewHarness", "TestRealGitFetch",
                       "TestCopilotDrive"}, sides)

    def test_failure_names_and_assertions_are_reported_by_side(self):
        for name in ("TestFederationMail", "TestLifecycle"):
            with self.subTest(name=name):
                result, _ = self.run_fixture(name)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("assertion details", result.stdout)
                self.assertIn("Failing tests", result.stdout)
                self.assertIn(name, result.stdout)
                for side in ("federation", "dashboard", "other"):
                    self.assertIn(f"=== agentd {side}:", result.stdout)

    def test_build_failure_stops_before_the_test_processes(self):
        result, _ = self.run_fixture(build_fail="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("compile error details", result.stdout)
        self.assertNotIn("=== agentd", result.stdout)


if __name__ == "__main__":
    unittest.main()
