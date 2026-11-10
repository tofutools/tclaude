#!/usr/bin/env python3
"""Decide which PR CI jobs a change needs. Every doubt resolves to running more.

Usage: classify-changes.py <base> [<head>] [--labels L1,L2] [--pr-head SHA]
                           [--self-hosted-macos]

Prints `key=value` lines for $GITHUB_OUTPUT (and the reasons to stderr):

  mode            docs | ui | code | full
  run_<job>       true | false, one per gated ci.yml job (see JOBS)
  test_matrix     JSON {"include": [{"os": ..., "shard": ...}, ...]}
  macos_runner    JSON runs-on value of the macOS jobs (a label or a list)
  expected        JSON {job: true|false}; ci-complete checks every gated job
                  against it, so a job may only be skipped when this said so

Modes:

  docs  every path is documentation. Only the always-on jobs run.
  ui    every path is documentation, a non-Go dashboard / jstest asset, or
        one of agentd's dashboard_*_test.go asset/HTML tests. The agentd,
        jstest and rest test shards run (they hold every test that reads the
        embedded assets, graded by scripts/ci/affected), plus lint when a Go
        test changed; every smoke is skipped.
  code  anything else that is a known source path. The test shards run in
        full; the proxy smokes and the harness CLI smokes run only when their
        rules below match.
  full  CI itself, the module graph, an unknown path, an empty diff, or any
        error. Everything runs (macOS too, with the opt-in below).

PR CI runs on Linux only unless the PR opts into macOS. Hosted macOS runners
are concurrency-capped, and starved macOS jobs blocked PRs, so darwin is
measured by release.yml on every merge to main instead (macOS agentd +
rest-all tests, the Seatbelt smoke and the darwin build). release.yml also
runs every job regardless of the diff, so a wrong skip here, or a darwin-only
break, delays a failure to the merge; it never ships one.

macOS opt-in: the PR carries the `ci:macos` label (--labels), or the PR's head
commit (--pr-head) has a `CI-macOS: true` trailer. Either one adds the macOS
agentd + rest-all test shards and the Seatbelt smoke wherever the test job
runs (ui, code and full; a docs-only change runs no tests), and `expected`
then requires them. A trailer on any other commit of the PR counts for
nothing: the opt-in is per head commit.

macOS runner: the opted-in jobs run on the hosted `macos-latest` unless
--self-hosted-macos is given, which ci.yml passes only when the PR's head
repository is this repository. A fork PR (whose trailer anyone can write)
therefore never reaches the self-hosted runner through this script.
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
# proxy-posture smokes exercise end to end.
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
# The self-hosted runner's default label set. Only same-repository PRs get it
# (--self-hosted-macos); forks and release.yml stay on the hosted MACOS.
MACOS_SELF_HOSTED = ["self-hosted", "macOS", "ARM64"]
LINUX_SHARDS = ("agentd", "jstest", "process", "sandbox", "rest")
UI_SHARDS = ("agentd", "jstest", "rest")
# An opted-in PR's macOS jobs: the same pair release.yml's macOS job runs.
# macOS folds process/sandbox/rest into one rest-all job and skips jstest (see
# the comments on the test job in ci.yml).
MACOS_SHARDS = ("agentd", "rest-all")

# Jobs that run only on the macOS opt-in, even in a full run. ci-complete's
# "a full run may not skip any job" check exempts exactly these.
OPT_IN_JOBS = ("sandbox-v2-seatbelt-smoke",)

MACOS_LABEL = "ci:macos"
MACOS_TRAILER = "CI-macOS"

# Every ci.yml job ci-complete must account for, besides the always-on ones
# (changes, docs, learn-commands). `test` is one entry: its matrix shrinks
# instead of being skipped.
JOBS = ("lint", "sandbox-v2-smoke", "sandbox-v2-seatbelt-smoke", "test",
        "copilot-smoke", "gemini-smoke", "pi-smoke", "filtered-proxy-smoke",
        "proxy-posture-e2e")


def is_doc(path):
    # docs/ is also a Go package (docs/embed.go embeds the tree for the
    # knowledge base); its Go source is code.
    if path.endswith(".go"):
        return False
    # These published artifacts also drive schema/signature package tests.
    if path.startswith(("docs/paf/schema/", "docs/paf/examples/")):
        return False
    # Markdown outside the docs tree may be embedded runtime input (skills).
    return (path.startswith("docs/") or path == "mkdocs.yml"
            or ("/" not in path and path.endswith(".md")))


def is_full(path):
    return path.startswith(FULL_PREFIXES) or path in FULL_EXACT


def is_known(path):
    return (is_doc(path) or path.startswith(CODE_PREFIXES) or path in CODE_EXACT
            or (path.startswith("docs/") and path.endswith(".go")))


def is_ui(path):
    return path.startswith(UI_PREFIXES) and not path.endswith(".go")


def is_ui_go_test(path):
    """agentd's dashboard_*_test.go: Go tests over the embedded assets and
    the dashboard HTML, which UI changes routinely edit alongside the assets.
    They run in the agentd shard; lint covers them."""
    return bool(UI_GO_TEST.match(path))


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


def macos_runner(self_hosted):
    """The runs-on value of the opted-in macOS jobs."""
    return list(MACOS_SELF_HOSTED) if self_hosted else MACOS


def classify(paths, affected, macos=False, self_hosted=False):
    """classify_paths, plus the macOS jobs when the PR opted in (macos), on
    the self-hosted runner only when self_hosted (a same-repository PR)."""
    mode, run, matrix, reasons = classify_paths(paths, affected)
    run["sandbox-v2-seatbelt-smoke"] = bool(macos and run["test"])
    if run["sandbox-v2-seatbelt-smoke"]:
        runner = macos_runner(self_hosted)
        matrix = matrix + [{"os": runner, "shard": s} for s in MACOS_SHARDS]
        reasons.append("macOS: opted in; the macOS test shards and the Seatbelt smoke run on "
                       + ("the self-hosted runner" if self_hosted else MACOS))
    elif macos:
        reasons.append("macOS: opted in, but no test job runs for this change")
    else:
        reasons.append(f"macOS: not opted in (no {MACOS_LABEL} label or "
                       f"{MACOS_TRAILER}: true trailer on the head commit)")
    return mode, run, matrix, reasons


def macos_opt_in(labels, trailer_values):
    """True when the PR has the label or the head commit says CI-macOS: true."""
    return (MACOS_LABEL in labels
            or any(v.strip().lower() == "true" for v in trailer_values))


def classify_paths(paths, affected):
    """Pure decision over the changed paths, macOS aside.

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
    if all(is_doc(p) or is_ui(p) or is_ui_go_test(p) for p in paths):
        run = {j: False for j in JOBS}
        run["test"] = True
        go_tests = [p for p in paths if is_ui_go_test(p)]
        run["lint"] = bool(go_tests)
        return "ui", run, [{"os": LINUX, "shard": s} for s in UI_SHARDS], [
            "every path is documentation, a dashboard/jstest asset or an agentd"
            " dashboard_*_test.go" + (f" (lint: {go_tests[0]})" if go_tests else "")]

    code = [p for p in paths if not is_doc(p)]
    run = {"lint": True, "sandbox-v2-smoke": True, "test": True}

    sandbox = [p for p in code if is_sandbox(p)]
    run["filtered-proxy-smoke"] = run["proxy-posture-e2e"] = bool(sandbox)
    reasons.append(f"proxy smokes: {sandbox[0]} is sandbox/proxy code"
                   if sandbox else "proxy smokes: no sandbox/proxy path changed")

    for job, pkg in HARNESS_SMOKES.items():
        run[job] = affected(pkg)
        reasons.append(f"{job}: change {'reaches' if run[job] else 'does not reach'} {pkg}")

    matrix = [{"os": LINUX, "shard": s} for s in LINUX_SHARDS]
    return "code", run, matrix, reasons


def full(reason):
    run = {j: True for j in JOBS}
    matrix = [{"os": LINUX, "shard": s} for s in LINUX_SHARDS]
    return "full", run, matrix, [reason]


# ---------------------------------------------------------------------------
# Git plumbing.

def git(*args, check=True):
    return subprocess.run(["git", *args], capture_output=True, check=check)


def changed_paths(base, head):
    out = git("diff", "--name-only", "--no-renames", "-z", base, head).stdout
    return [p.decode() for p in out.split(b"\0") if p]


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


def head_trailers(sha):
    """The CI-macOS trailer values of one commit's message."""
    out = git("log", "-1", f"--format=%(trailers:key={MACOS_TRAILER},valueonly,separator=%x00)",
              f"{sha}^{{commit}}").stdout.decode()
    return [v for v in out.strip("\n").split("\0") if v.strip()]


def parse_args(argv):
    positional, labels, pr_head, self_hosted = [], [], None, False
    args = iter(argv[1:])
    for arg in args:
        if arg == "--self-hosted-macos":
            self_hosted = True
        elif arg in ("--labels", "--pr-head"):
            value = next(args, None)
            if value is None:
                sys.exit(__doc__)
            if arg == "--labels":
                labels += [x.strip() for x in value.replace("\n", ",").split(",") if x.strip()]
            else:
                pr_head = value
        else:
            positional.append(arg)
    if len(positional) not in (1, 2):
        sys.exit(__doc__)
    return (positional[0], positional[1] if len(positional) > 1 else "HEAD",
            labels, pr_head, self_hosted)


def main(argv):
    base, head, labels, pr_head, self_hosted = parse_args(argv)
    try:
        # Resolve both ends first: a missing base must fail, not look empty.
        git("rev-parse", "--verify", f"{base}^{{commit}}")
        git("rev-parse", "--verify", f"{head}^{{commit}}")
        head = git("rev-parse", head).stdout.decode().strip()
        paths = changed_paths(base, head)
        trailers = head_trailers(pr_head) if pr_head else []
        mode, run, matrix, reasons = classify(paths, make_affected(base, head),
                                              macos=macos_opt_in(labels, trailers),
                                              self_hosted=self_hosted)
    except subprocess.CalledProcessError as err:
        sys.stderr.write(f"classify-changes: {err} {err.stderr!r}\n")
        sys.exit(1)
    for reason in reasons:
        print(f"classify-changes: {reason}", file=sys.stderr)
    print(f"mode={mode}")
    for job in JOBS:
        print(f"run_{job.replace('-', '_')}={'true' if run[job] else 'false'}")
    print("test_matrix=" + json.dumps({"include": matrix}, separators=(",", ":")))
    print("macos_runner=" + json.dumps(macos_runner(self_hosted), separators=(",", ":")))
    print("expected=" + json.dumps(run, separators=(",", ":"), sort_keys=True))


if __name__ == "__main__":
    main(sys.argv)
