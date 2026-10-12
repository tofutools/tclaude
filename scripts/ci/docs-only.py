#!/usr/bin/env python3
"""Print true only when every changed path is documentation; errors fail closed."""
import subprocess
import sys

paths = subprocess.check_output([
    "git", "diff", "--name-only", "--no-renames", "-z", sys.argv[1],
    sys.argv[2] if len(sys.argv) > 2 else "HEAD",
]).split(b"\0")
paths = [path for path in paths if path]
def is_doc(path):
    # These published artifacts also drive schema/signature package tests.
    if path.startswith((b"docs/paf/schema/", b"docs/paf/examples/")):
        return False
    # Markdown outside the docs tree may be embedded runtime input (skills).
    return (path.startswith(b"docs/") or path == b"mkdocs.yml"
            or (b"/" not in path and path.endswith(b".md")))


print("true" if paths and all(map(is_doc, paths)) else "false")
