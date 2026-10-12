#!/usr/bin/env python3
"""Print true only when every changed path is documentation; errors fail closed."""
import subprocess
import sys

paths = subprocess.check_output([
    "git", "diff", "--name-only", "--no-renames", "-z", sys.argv[1],
    sys.argv[2] if len(sys.argv) > 2 else "HEAD",
]).split(b"\0")
paths = [path for path in paths if path]
print("true" if paths and all(
    path.startswith(b"docs/") or path.endswith(b".md") or path == b"mkdocs.yml"
    for path in paths
) else "false")
