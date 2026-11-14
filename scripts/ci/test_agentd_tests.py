"""The agentd runner must cover every test exactly once across concurrent
processes, report failures readably, and turn a hang into a fast, named
failure."""
import os
import subprocess
import tempfile
import time
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

# `go test -c -o BIN` writes FAKE_BINARY to BIN; `go tool test2json` is an
# unbuffered cat (the fake binary already speaks test2json's JSON).
FAKE_GO = r'''#!/usr/bin/env python3
import os, sys
from pathlib import Path
args = sys.argv[1:]
root = Path(os.environ["FAKE_GO_STATE"])
if args[:2] == ["tool", "test2json"]:
    # Like the real converter, write each event as it arrives.
    for line in sys.stdin.buffer:
        sys.stdout.buffer.write(line)
        sys.stdout.buffer.flush()
    sys.exit(0)
if args[:2] != ["test", "-c"]:
    print("unexpected go invocation", args, file=sys.stderr)
    sys.exit(2)
if os.getenv("FAKE_GO_BUILD_FAIL"):
    print("compile error details")
    sys.exit(1)
out = Path(args[args.index("-o") + 1])
out.write_text((root / "fake_binary").read_text())
out.chmod(0o755)
(root / "build_args").write_text(" ".join(args))
'''

FAKE_BINARY = r'''#!/usr/bin/env python3
import json, os, signal, sys, time
from pathlib import Path
root = Path(os.environ["FAKE_GO_STATE"])
names = ["TestDashboardNewFeature", "TestFederationMail", "TestLifecycle",
         "TestFutureFeature", "TestSpawnNewHarness", "TestPiDrive",
         "TestRealGitFetch", "ExampleCLI", "FuzzParser"]
if "-test.list" in sys.argv:
    print("\n".join(names + ["BenchmarkIgnored"]))
    sys.exit(0)
partition = os.environ["TCLAUDE_AGENTD_TEST_PARTITION"]
selected = Path(partition).read_text().split()
me = Path(partition).stem
(root / (me + ".ran")).write_text("\n".join(selected))
(root / (me + ".args")).write_text(" ".join(sys.argv[1:]))
# Every process must be running at the same time.
procs = int(os.environ["TCLAUDE_AGENTD_TEST_PROCS"])
deadline = time.monotonic() + 5
while len(list(root.glob("*.ran"))) < procs:
    if time.monotonic() > deadline:
        print("test processes did not run concurrently", file=sys.stderr)
        sys.exit(1)
    time.sleep(.005)
def emit(**event):
    print(json.dumps(event), flush=True)
def dump(*_):
    emit(Action="output", Output="goroutine 1 [chan receive]:\nfake dump\n")
    sys.exit(2)
signal.signal(signal.SIGQUIT, dump)
if os.getenv("FAKE_LEAK") and me == "p1":
    # A child that inherits the output pipe and outlives the binary.
    import subprocess
    subprocess.Popen(["sleep", "20"], start_new_session=True)
failed = False
for name in selected:
    emit(Action="run", Test=name)
    emit(Action="output", Test=name, Output="=== RUN   " + name + "\n")
    if name == os.getenv("FAKE_HANG"):
        emit(Action="output", Test=name, Output="assertion before the hang\n")
        while True:
            time.sleep(1)
    if name == os.getenv("FAKE_FAIL"):
        emit(Action="output", Test=name, Output="assertion details\n")
        emit(Action="fail", Test=name, Elapsed=0.5)
        failed = True
    else:
        emit(Action="pass", Test=name, Elapsed=0.01)
emit(Action="fail" if failed else "pass", Elapsed=.01)
if os.getenv("FAKE_TEARDOWN_HANG") and me == "p1":
    while True:  # a TestMain teardown that never returns
        time.sleep(1)
sys.exit(1 if failed else 0)
'''

ALL = {"TestDashboardNewFeature", "TestFederationMail", "TestLifecycle", "TestFutureFeature",
       "TestSpawnNewHarness", "TestPiDrive", "TestRealGitFetch", "ExampleCLI", "FuzzParser"}


class RunnerTests(unittest.TestCase):
    def run_fixture(self, *args, procs=3, fail="", hang="", build_fail="", leak="", teardown_hang=""):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            (root / "go").write_text(FAKE_GO)
            (root / "go").chmod(0o755)
            (root / "fake_binary").write_text(FAKE_BINARY)
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"],
                       FAKE_GO_STATE=str(root), FAKE_FAIL=fail, FAKE_HANG=hang,
                       FAKE_GO_BUILD_FAIL=build_fail, FAKE_LEAK=leak,
                       FAKE_TEARDOWN_HANG=teardown_hang,
                       TCLAUDE_AGENTD_TEST_DRAIN_GRACE="1",
                       TCLAUDE_AGENTD_TEST_PROCESS_GRACE="1",
                       TCLAUDE_AGENTD_TEST_PROCS=str(procs),
                       TCLAUDE_AGENTD_TEST_WATCHDOG="1",
                       TCLAUDE_AGENTD_TEST_LOG_DIR=str(root / "logs"))
            env.pop("GITHUB_STEP_SUMMARY", None)
            result = subprocess.run(["python3", "scripts/ci/agentd-tests.py", *args],
                                    cwd=ROOT, env=env, text=True, capture_output=True, timeout=60)
            ran = [set((p).read_text().split()) for p in sorted(root.glob("*.ran"))]
            run_args = [(p).read_text() for p in sorted(root.glob("*.args"))]
            build_args = (root / "build_args").read_text() if (root / "build_args").exists() else ""
            return result, ran, run_args, build_args

    def test_every_test_runs_exactly_once_across_concurrent_processes(self):
        result, ran, run_args, build_args = self.run_fixture("-count=1", "-race", "-timeout=7m")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(ran), 3)
        self.assertEqual(set().union(*ran), ALL)
        self.assertEqual(sum(len(r) for r in ran), len(ALL), "a test ran in two processes")
        self.assertIn("-race", build_args)
        for args in run_args:
            self.assertIn("-test.timeout=7m", args)
        self.assertIn("Slowest tests", result.stdout)

    def test_failure_names_and_assertions_are_reported_by_process(self):
        result, _, _, _ = self.run_fixture(fail="TestLifecycle")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("--- failed test: TestLifecycle ---", result.stdout)
        self.assertIn("assertion details", result.stdout)
        self.assertIn("Failing tests", result.stdout)

    def test_a_hang_fails_fast_and_named_with_a_goroutine_dump(self):
        began = time.monotonic()
        result, _, _, _ = self.run_fixture(hang="TestFederationMail")
        self.assertLess(time.monotonic() - began, 30, "the watchdog did not stop the hung process")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("::error title=agentd test hang::TestFederationMail", result.stdout)
        self.assertIn("still running when the process ended: TestFederationMail", result.stdout)
        self.assertIn("assertion before the hang", result.stdout)
        self.assertIn("fake dump", result.stdout)
        self.assertIn("Failing tests", result.stdout)

    def test_a_leaked_child_holding_the_output_cannot_hang_the_runner(self):
        began = time.monotonic()
        result, _, _, _ = self.run_fixture(leak="1")
        self.assertLess(time.monotonic() - began, 30)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("output stayed open", result.stdout)

    def test_a_hang_outside_any_test_is_stopped_by_the_process_deadline(self):
        began = time.monotonic()
        result, _, _, _ = self.run_fixture("-timeout=2s", teardown_hang="1")
        self.assertLess(time.monotonic() - began, 30)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("outside any test", result.stdout)
        self.assertIn("fake dump", result.stdout)

    def test_build_failure_stops_before_the_test_processes(self):
        result, ran, _, _ = self.run_fixture(build_fail="1")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("compile error details", result.stdout)
        self.assertEqual(ran, [])

    def test_selection_flags_are_refused(self):
        for flag in ("-run=TestX", "-skip=TestX", "-coverprofile=x"):
            with self.subTest(flag=flag):
                result, ran, _, _ = self.run_fixture(flag)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(ran, [])


if __name__ == "__main__":
    unittest.main()
