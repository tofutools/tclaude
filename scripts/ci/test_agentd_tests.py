"""The runner must cover both sides concurrently and retain readable failures."""
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
args = sys.argv
flag = "-run" if "-run" in args else "-skip"
pattern = args[args.index(flag)+1]
side = "domain" if flag == "-run" else "other"
root = Path(os.environ["FAKE_GO_STATE"])
(root / side).touch()
deadline = time.monotonic()+2
while not (root / ("other" if side=="domain" else "domain")).exists():
    if time.monotonic() > deadline:
        print("test processes did not run concurrently", file=sys.stderr)
        sys.exit(1)
    time.sleep(.005)
names = ["TestDashboardNewFeature", "TestFederationMail", "TestLifecycle",
         "TestFutureFeature", "ExampleCLI", "FuzzParser"]
selected = [name for name in names if bool(re.match(pattern, name)) == (flag=="-run")]
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
    def run_fixture(self, failing=""):
        with tempfile.TemporaryDirectory() as root:
            root = Path(root)
            (root / "go").write_text(FAKE_GO)
            (root / "go").chmod(0o755)
            env = dict(os.environ, PATH=str(root)+os.pathsep+os.environ["PATH"],
                       FAKE_GO_STATE=str(root), FAKE_GO_FAIL=failing,
                       TCLAUDE_AGENTD_TEST_LOG_DIR=str(root / "logs"))
            result = subprocess.run(["bash", "scripts/ci/agentd-tests.sh", "-count=1"],
                                    cwd=ROOT, env=env, text=True, capture_output=True,
                                    timeout=10)
            domain = set((root / "domain.names").read_text().splitlines())
            other = set((root / "other.names").read_text().splitlines())
            return result, domain, other

    def test_concurrent_exact_complement_includes_future_tests_and_examples(self):
        result, domain, other = self.run_fixture()
        self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
        self.assertFalse(domain & other)
        self.assertEqual(domain | other, {"TestDashboardNewFeature", "TestFederationMail",
                         "TestLifecycle", "TestFutureFeature", "ExampleCLI", "FuzzParser"})

    def test_failure_names_and_assertions_are_reported_by_side(self):
        for name in ("TestFederationMail", "TestLifecycle"):
            with self.subTest(name=name):
                result, _, _ = self.run_fixture(name)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("assertion details", result.stdout)
                self.assertIn("Failing tests", result.stdout)
                self.assertIn(name, result.stdout)
                self.assertIn("=== agentd domain:", result.stdout)
                self.assertIn("=== agentd other:", result.stdout)


if __name__ == "__main__":
    unittest.main()
