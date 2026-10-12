"""Exercise the actual git-diff classifier in an isolated repository."""
import pathlib
import subprocess
import tempfile
import unittest

SCRIPT = pathlib.Path(__file__).with_name("docs-only.py").resolve()


class DocsOnlyTest(unittest.TestCase):
    def test_diff_paths(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)

            def git(*args):
                return subprocess.check_output(["git", *args], cwd=root)

            git("init", "--quiet")
            git("config", "user.email", "fixture@example.invalid")
            git("config", "user.name", "Fixture")
            (root / "main.go").write_text("package main\n")
            git("add", "main.go")
            git("commit", "--quiet", "-m", "base")
            base = git("rev-parse", "HEAD").decode().strip()

            def classify(expected):
                actual = subprocess.check_output(
                    ["python3", str(SCRIPT), base], cwd=root, text=True
                ).strip()
                self.assertEqual(actual, expected)

            classify("false")  # Empty diffs must not skip evidence.
            for name in ("docs/learn/guide.md", "docs/assets/image.png",
                         "README.md", "nested/notes.md", "mkdocs.yml",
                         "docs/a name\nwith newline.png"):
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("fixture\n")
                git("add", name)
            git("commit", "--quiet", "-m", "docs")
            classify("true")
            (root / "main.go").write_text("package changed\n")
            git("add", "main.go")
            git("commit", "--quiet", "-m", "mixed")
            classify("false")
            git("reset", "--hard", base)
            git("mv", "main.go", "README.md")
            git("commit", "--quiet", "-m", "code renamed into docs")
            classify("false")
            git("reset", "--hard", base)
            git("rm", "main.go")
            git("commit", "--quiet", "-m", "code deletion")
            classify("false")
            failed = subprocess.run(
                ["python3", str(SCRIPT), "missing-base"], cwd=root,
                capture_output=True, text=True,
            )
            self.assertNotEqual(failed.returncode, 0)
            self.assertNotEqual(failed.stdout.strip(), "true")


if __name__ == "__main__":
    unittest.main()
