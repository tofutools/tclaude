#!/usr/bin/env python3
"""Decide which PR CI jobs a change needs. Every doubt resolves to running more.

Usage: classify-changes.py <base> [<head>]

Prints `key=value` lines for $GITHUB_OUTPUT (and the reasons to stderr):

  mode            docs | ui | code | full
  run_<job>       true | false, one per gated ci.yml job (see JOBS)
  test_matrix     JSON {"include": [{"os": ..., "shard": ...}, ...]}
  expected        JSON {job: true|false}; ci-complete checks every gated job
                  against it, so a job may only be skipped when this said so

Modes:

  docs  every path is documentation. Only the always-on jobs run.
  ui    every path is documentation, a non-Go dashboard / jstest asset, or
        one of agentd's dashboard_*_test.go asset/HTML tests. The Linux
        agentd, jstest and rest test shards run (they hold every test that
        reads the embedded assets, graded by scripts/ci/affected), plus lint
        when a Go test changed; every smoke and every macOS job is skipped.
  code  anything else that is a known source path. Linux runs in full; the
        macOS jobs, the sandbox/proxy smokes and the harness CLI smokes run
        only when their rules below match.
  full  CI itself, the module graph, an unknown path, an empty diff, or any
        error. Everything runs, macOS included.

release.yml runs every job on every merge to main regardless, so a wrong skip
here delays a failure to the merge; it never ships one.
"""
import json
import re
import subprocess
import sys

# ---------------------------------------------------------------------------
# Path rules. Paths are repository-relative with forward slashes.

# Changing any of these changes what CI itself does or what every package is
# built against, so nothing about the diff can be trusted to narrow the run.
FULL_PREFIXES = (".github/", "scripts/ci/")
FULL_EXACT = {"go.mod", "go.sum", "mise.toml", "scripts/test.sh"}

# Known non-doc source locations. A path matching none of these, and not a doc,
# is unknown and forces a full run.
CODE_PREFIXES = ("pkg/", "cmd/", "scripts/", "skills/", ".claude/",
                 # Not docs: schema/signature package tests read these.
                 "docs/paf/schema/", "docs/paf/examples/")
CODE_EXACT = {"main.go", ".golangci.yml", ".gitignore", ".gitattributes",
              "LICENSE", "renovate.json"}

# Dashboard assets and their Node suites. Go files under these trees are code.
UI_PREFIXES = ("pkg/claude/agentd/dashboard/", "pkg/claude/agentd/jstest/")
UI_GO_TEST = re.compile(r"^pkg/claude/agentd/dashboard_[^/]*_test\.go$")

# Sandbox, proxy, session and harness-launch code: what the filtered-proxy and
# proxy-posture smokes and the macOS Seatbelt smoke exercise end to end.
SANDBOX_PREFIXES = (
    "pkg/claude/session/",
    "pkg/claude/proxy/",
    "pkg/claude/sandboxproxy/",
    "pkg/claude/common/sandboxpolicy/",
    "pkg/claude/sandboxassumptions/",
    "pkg/claude/harness/",
    "pkg/claude/opencodeapi/",
    "pkg/claude/codexappserver/",
    "pkg/harnesscredentials/",
    "pkg/harnessops/",
    "pkg/common/harnesspath/",
    "scripts/filtered-proxy-smoke/",
    "scripts/proxy-posture-e2e/",
    "scripts/seatbelt-smoke/",
    "scripts/lib/",
)
# The harness CLI fixtures measure third-party binaries and have their own
# smokes (graded by import graph below); they do not drive the sandbox.
SANDBOX_EXCLUDED = re.compile(r"^pkg/claude/harness/[a-z]+fixture/")
# Sandbox-shaped files living in otherwise general packages (agentd's OpenCode
# proxy runtime, for instance).
SANDBOX_NAME = re.compile(
    r"sandbox|proxy|seatbelt|netns|bwrap|bubblewrap|passt|egress|firewall"
    r"|network|opencode", re.IGNORECASE)

# Go's GOOS values plus `unix`, which this repo uses as a build tag and file
# suffix. A file naming any of them, or `cgo`, behaves differently per OS.
GOOS = ("aix android darwin dragonfly freebsd hurd illumos ios js linux nacl "
        "netbsd openbsd plan9 solaris wasip1 windows zos unix").split()
GOARCH = ("386 amd64 arm arm64 loong64 mips mips64 mips64le mipsle ppc64 "
          "ppc64le riscv64 s390x wasm").split()
BUILD_LINE = re.compile(r"^//go:build (.*)$|^// \+build (.*)$", re.MULTILINE)
OS_WORD = re.compile(r"\b(?:%s|cgo)\b" % "|".join(GOOS))
CGO_IMPORT = re.compile(r'^import "C"$|^import \(\s*"C"', re.MULTILINE)
# Non-Go files that are platform-specific by name (testdata baselines, Seatbelt
# profiles) or are native sources cgo compiles.
PLATFORM_NAME = re.compile(r"darwin|macos|seatbelt|\.sb$", re.IGNORECASE)
NATIVE_EXT = (".c", ".h", ".m", ".mm", ".s", ".S", ".swift", ".cc", ".cpp")
# Packages too large and too frequently edited for "a platform file lives here"
# to mean "every edit here is platform-relevant". Only their platform-specific
# files themselves trigger macOS.
PER_FILE_ONLY_DIRS = {
    "pkg/claude/agentd",
    "pkg/claude/agentd/jstest",
    "pkg/claude/harness",
    "pkg/claude/harness/copilotfixture",
}

# Harness CLI smokes: job -> the Go package whose tests are the smoke. The
# smoke is needed exactly when scripts/ci/affected says the change reaches
# that package's test closure.
HARNESS_SMOKES = {
    "copilot-smoke": "./pkg/claude/harness/copilotfixture",
    "gemini-smoke": "./pkg/claude/harness/geminifixture",
    "pi-smoke": "./pkg/claude/harness/pifixture",
}

LINUX = "ubuntu-latest"
MACOS = "macos-latest"
LINUX_SHARDS = ("agentd", "jstest", "process", "sandbox", "rest")
UI_SHARDS = ("agentd", "jstest", "rest")
# macOS PR CI folds process/sandbox/rest into one rest-all job and skips jstest
# (see the comments on the test job in ci.yml).
MACOS_SHARDS = ("agentd", "rest-all")

# Every ci.yml job ci-complete must account for, besides the always-on ones
# (changes, docs, learn-commands). `test` is one entry: its matrix shrinks
# instead of being skipped.
JOBS = ("lint", "sandbox-v2-smoke", "sandbox-v2-seatbelt-smoke", "test",
        "copilot-smoke", "gemini-smoke", "pi-smoke", "filtered-proxy-smoke",
        "proxy-posture-e2e")


def is_doc(path):
    # These published artifacts also drive schema/signature package tests.
    if path.startswith(("docs/paf/schema/", "docs/paf/examples/")):
        return False
    # Markdown outside the docs tree may be embedded runtime input (skills).
    return (path.startswith("docs/") or path == "mkdocs.yml"
            or ("/" not in path and path.endswith(".md")))


def is_full(path):
    return path.startswith(FULL_PREFIXES) or path in FULL_EXACT


def is_known(path):
    return is_doc(path) or path.startswith(CODE_PREFIXES) or path in CODE_EXACT


def is_ui(path):
    return path.startswith(UI_PREFIXES) and not path.endswith(".go")


def is_ui_go_test(path, read):
    """agentd's dashboard_*_test.go: Go tests over the embedded assets and
    the dashboard HTML, which UI changes routinely edit alongside the assets.
    They run in the Linux agentd shard; lint covers them. A platform-specific
    one is code."""
    return (bool(UI_GO_TEST.match(path)) and not has_os_suffix(path)
            and not is_platform_file(path, read))


def is_sandbox(path):
    if SANDBOX_EXCLUDED.match(path):
        return False
    if path.startswith(SANDBOX_PREFIXES):
        return True
    # The keyword is about Go/package code, not a dashboard asset or a
    # dashboard HTML test that happens to render a "Sandbox" panel.
    if path.startswith(UI_PREFIXES) or UI_GO_TEST.match(path):
        return False
    return path.startswith("pkg/") and bool(SANDBOX_NAME.search(basename(path)))


def basename(path):
    return path.rsplit("/", 1)[-1]


def dirname(path):
    return path.rsplit("/", 1)[0] if "/" in path else ""


def has_os_suffix(path):
    """Go's filename build constraints: name_GOOS, name_GOARCH, name_GOOS_GOARCH
    (each optionally followed by _test), plus this repo's _unix convention."""
    if not path.endswith(".go"):
        return False
    parts = basename(path)[:-3].split("_")
    if parts and parts[-1] == "test":
        parts = parts[:-1]
    if len(parts) < 2:
        return False
    return parts[-1] in GOOS or parts[-1] in GOARCH or (
        len(parts) >= 3 and parts[-2] in GOOS and parts[-1] in GOARCH)


def go_source_is_platform(text):
    for match in BUILD_LINE.finditer(text):
        if OS_WORD.search(match.group(1) or match.group(2) or ""):
            return True
    return bool(CGO_IMPORT.search(text))


def is_platform_file(path, read):
    """True when the file compiles or behaves differently per OS. `read`
    returns the file's text (head, or base when deleted), or None."""
    name = basename(path)
    if name.endswith(NATIVE_EXT) or PLATFORM_NAME.search(name):
        return True
    if not path.endswith(".go"):
        return False
    if has_os_suffix(path):
        return True
    text = read(path)
    # Unreadable Go source is a doubt; doubts run macOS.
    return text is None or go_source_is_platform(text)


def platform_dirs(go_files, read):
    """Package directories holding any platform-specific Go file, minus the
    per-file-only packages. Any change in one of these runs macOS."""
    dirs = set()
    for path in go_files:
        d = dirname(path)
        if d in dirs or d in PER_FILE_ONLY_DIRS:
            continue
        if is_platform_file(path, read):
            dirs.add(d)
    return dirs


def owner_dir(path, go_dirs):
    """The package directory a file belongs to: the nearest directory at or
    above it holding Go source (testdata and embedded assets belong to the
    package that reads them), or "" at the module root."""
    d = dirname(path)
    while d and d not in go_dirs:
        d = dirname(d)
    return d


def needs_macos(path, read, plat_dirs, go_dirs):
    if is_sandbox(path) or is_platform_file(path, read):
        return True
    return owner_dir(path, go_dirs) in plat_dirs


def classify(paths, read, go_files, affected):
    """Pure decision over the changed paths.

    read(path) -> str|None: the file's text (head, else base).
    go_files: every .go path in the head tree (for platform_dirs).
    affected(pkg) -> bool: does the change reach pkg's test closure; must
        return True when it cannot tell.
    Returns (mode, run: {job: bool}, matrix: [ {os, shard} ], reasons: [str]).
    """
    reasons = []
    paths = sorted(set(paths))
    if not paths:
        return full("empty diff")
    for path in paths:
        if is_full(path):
            return full(f"{path} changes CI or the module graph")
        if not is_known(path):
            return full(f"{path} is not a known source location")
    if all(is_doc(p) for p in paths):
        # The skipped test job still parses its matrix expression; give it a
        # well-formed one that is never run.
        return "docs", {j: False for j in JOBS}, [{"os": LINUX, "shard": "none"}], [
            "every path is documentation"]
    if all(is_doc(p) or is_ui(p) or is_ui_go_test(p, read) for p in paths):
        run = {j: False for j in JOBS}
        run["test"] = True
        go_tests = [p for p in paths if is_ui_go_test(p, read)]
        run["lint"] = bool(go_tests)
        return "ui", run, [{"os": LINUX, "shard": s} for s in UI_SHARDS], [
            "every path is documentation, a dashboard/jstest asset or an agentd"
            " dashboard_*_test.go" + (f" (lint: {go_tests[0]})" if go_tests else "")]

    code = [p for p in paths if not is_doc(p)]
    run = {"lint": True, "sandbox-v2-smoke": True, "test": True}

    plat_dirs = platform_dirs(go_files, read)
    go_dirs = {dirname(p) for p in go_files}
    mac = [p for p in code if needs_macos(p, read, plat_dirs, go_dirs)]
    run["sandbox-v2-seatbelt-smoke"] = bool(mac)
    reasons.append(f"macOS: {mac[0]} is platform/sandbox code" if mac
                   else "macOS: no platform-specific or sandbox path changed")

    sandbox = [p for p in code if is_sandbox(p)]
    run["filtered-proxy-smoke"] = run["proxy-posture-e2e"] = bool(sandbox)
    reasons.append(f"proxy smokes: {sandbox[0]} is sandbox/proxy code"
                   if sandbox else "proxy smokes: no sandbox/proxy path changed")

    for job, pkg in HARNESS_SMOKES.items():
        run[job] = affected(pkg)
        reasons.append(f"{job}: change {'reaches' if run[job] else 'does not reach'} {pkg}")

    matrix = [{"os": LINUX, "shard": s} for s in LINUX_SHARDS]
    if mac:
        matrix += [{"os": MACOS, "shard": s} for s in MACOS_SHARDS]
    return "code", run, matrix, reasons


def full(reason):
    run = {j: True for j in JOBS}
    matrix = ([{"os": LINUX, "shard": s} for s in LINUX_SHARDS]
              + [{"os": MACOS, "shard": s} for s in MACOS_SHARDS])
    return "full", run, matrix, [reason]


# ---------------------------------------------------------------------------
# Git plumbing.

def git(*args, check=True):
    return subprocess.run(["git", *args], capture_output=True, check=check)


def changed_paths(base, head):
    out = git("diff", "--name-only", "--no-renames", "-z", base, head).stdout
    return [p.decode() for p in out.split(b"\0") if p]


def make_reader(base, head):
    cache = {}

    def read(path):
        if path not in cache:
            cache[path] = None
            for rev in (head, base):
                res = git("show", f"{rev}:{path}", check=False)
                if res.returncode == 0:
                    cache[path] = res.stdout.decode("utf-8", "replace")
                    break
        return cache[path]
    return read


def head_go_files(head):
    out = git("ls-tree", "-r", "-z", "--name-only", head).stdout
    return [p.decode() for p in out.split(b"\0") if p.endswith(b".go")]


def make_bulk_reader(base, head, go_files):
    """Prefetch every head .go file in one `git cat-file --batch` so scanning
    the tree for platform packages stays one subprocess."""
    texts = {}
    proc = subprocess.run(
        ["git", "cat-file", "--batch"],
        input="".join(f"{head}:{p}\n" for p in go_files).encode(),
        capture_output=True, check=True)
    data, pos = proc.stdout, 0
    for path in go_files:
        nl = data.index(b"\n", pos)
        header = data[pos:nl].split()
        pos = nl + 1
        if len(header) == 3:
            size = int(header[2])
            texts[path] = data[pos:pos + size].decode("utf-8", "replace")
            pos += size + 1
    fallback = make_reader(base, head)
    return lambda path: texts[path] if path in texts else fallback(path)


def make_affected(base, head):
    def affected(pkg):
        try:
            res = subprocess.run(
                ["go", "run", "./scripts/ci/affected", "-base", base, "-head", head, pkg],
                capture_output=True, text=True)
        except OSError as err:
            sys.stderr.write(f"classify-changes: cannot run affected: {err}\n")
            return True
        sys.stderr.write(res.stderr)
        if res.returncode != 0:
            return True
        return bool(res.stdout.strip())
    return affected


def main(argv):
    if len(argv) not in (2, 3):
        sys.exit(__doc__)
    base = argv[1]
    head = argv[2] if len(argv) > 2 else "HEAD"
    try:
        # Resolve both ends first: a missing base must fail, not look empty.
        git("rev-parse", "--verify", f"{base}^{{commit}}")
        git("rev-parse", "--verify", f"{head}^{{commit}}")
        head = git("rev-parse", head).stdout.decode().strip()
        paths = changed_paths(base, head)
        go_files = head_go_files(head)
        read = make_bulk_reader(base, head, go_files)
        mode, run, matrix, reasons = classify(
            paths, read, go_files, make_affected(base, head))
    except subprocess.CalledProcessError as err:
        sys.stderr.write(f"classify-changes: {err} {err.stderr!r}\n")
        sys.exit(1)
    for reason in reasons:
        print(f"classify-changes: {reason}", file=sys.stderr)
    print(f"mode={mode}")
    for job in JOBS:
        print(f"run_{job.replace('-', '_')}={'true' if run[job] else 'false'}")
    print("test_matrix=" + json.dumps({"include": matrix}, separators=(",", ":")))
    print("expected=" + json.dumps(run, separators=(",", ":"), sort_keys=True))


if __name__ == "__main__":
    main(sys.argv)
