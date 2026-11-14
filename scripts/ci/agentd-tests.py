#!/usr/bin/env python3
"""Run pkg/claude/agentd's tests as several concurrent processes of one binary.

Why: the package is one test binary of ~7000 serial flow tests whose wall
time is mostly spent waiting on fixtures (debounces, retry and poll delays),
not computing: a full run keeps barely more than one core busy. The tests keep
mutable fixture globals serial within a process, so the parallelism comes from
running several processes of the same binary, each over its own slice of the
test list.

How:
  1. Build the test binary once (`go test -c`). Linking is not cached by go,
     so running `go test` per process would relink it every time.
  2. List its top-level tests and deal them round-robin, in name order, into
     N partition files. Name order puts a family of similar tests (and similar
     cost) next to each other, so dealing spreads every family across all
     processes. New tests are picked up by the listing; nothing to maintain.
  3. Run one binary per partition (TestMain reads the file named by
     TCLAUDE_AGENTD_TEST_PARTITION), its output piped through test2json.
  4. Watch each process's event stream. A top-level test running longer than
     the watchdog limit is named on the console at once and its process gets
     SIGQUIT, which makes Go dump every goroutine and exit: a hang (classically
     a cleanup blocked after a failed assertion) fails within minutes, named,
     with stacks, instead of running into the package timeout.
  5. Report per partition after all processes finish: failing tests with their
     output, tests that never finished, and the slowest tests overall.

Arguments mirror the go test flags the CI action passes: -race (build),
-timeout=D (per process backstop), -count=N (accepted; a directly run binary
never reuses cached results). Selection flags are owned by this runner.

Environment:
  TCLAUDE_AGENTD_TEST_LOG_DIR   keep the binary and per-partition JSON here
  TCLAUDE_AGENTD_TEST_PROCS     process count (default: derived from cores)
  TCLAUDE_AGENTD_TEST_WATCHDOG  per-test limit in seconds (default 120)
  TCLAUDE_AGENTD_TEST_COVER=1   build with -cover, write <partition>.cover
"""
import json
import os
import platform
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
from pathlib import Path

PKG = "github.com/tofutools/tclaude/pkg/claude/agentd"
PKG_DIR = "pkg/claude/agentd"
ROOT = Path(__file__).resolve().parents[2]
TEST_NAME = re.compile(r"^(Test|Example|Fuzz)\w*$")
DEFAULT_WATCHDOG = 120.0
KILL_GRACE = 30.0
# After the binary exits, how long its output may stay open. A child process
# a test leaked keeps the binary's output pipe open and so test2json from
# ever seeing EOF (go test bounds this with its WaitDelay; so must we).
DRAIN_GRACE = float(os.environ.get("TCLAUDE_AGENTD_TEST_DRAIN_GRACE") or 10)
# Beyond -test.timeout, which is armed only inside m.Run: how long TestMain's
# setup and teardown may add before the whole process is stopped.
PROCESS_GRACE = float(os.environ.get("TCLAUDE_AGENTD_TEST_PROCESS_GRACE") or 60)
DEFAULT_TIMEOUT = 600.0
MAX_DEFAULT_PROCS = 16


def default_procs():
    # Measured with the suite pinned to 4 cores (the Linux runner's size):
    # tests took 128s in 6 processes, 106s in 8 and 95s in 12, against a
    # CPU floor near 80s (~325 CPU-seconds). Two per core takes most of the
    # gain without saturating the CPU that timing-sensitive fixtures share.
    # Usable cores, not the host's, and capped: a large self-hosted runner
    # would otherwise start dozens of flow-test processes.
    try:
        cores = len(os.sched_getaffinity(0))
    except AttributeError:  # macOS
        cores = os.cpu_count() or 2
    return max(2, min(2 * cores, MAX_DEFAULT_PROCS))


def go_duration(text):
    """Seconds in a go test -timeout value such as 10m, 90s or 1h30m."""
    units = {"h": 3600, "m": 60, "s": 1, "ms": 0.001}
    parts = re.findall(r"(\d+(?:\.\d+)?)(h|ms|m|s)", text)
    if not parts or "".join(n + u for n, u in parts) != text:
        raise SystemExit(f"agentd-tests: cannot parse -timeout={text}")
    return sum(float(n) * units[u] for n, u in parts)


def parse_args(argv):
    build, run, timeout = [], [], DEFAULT_TIMEOUT
    for arg in argv:
        if arg == "-race":
            build.append(arg)
        elif arg.startswith("-timeout="):
            value = arg.split("=", 1)[1]
            timeout = go_duration(value)
            run.append("-test.timeout=" + value)
        elif arg.startswith("-count="):
            pass
        else:
            raise SystemExit(f"agentd-tests: unsupported argument {arg!r} "
                             "(selection and profile flags are owned by this runner)")
    if not any(a.startswith("-test.timeout=") for a in run):
        run.append(f"-test.timeout={DEFAULT_TIMEOUT:.0f}s")
    return build, run, timeout


def alias_tmpdir(stack):
    """On Linux, mirror scripts/test.sh: a TMPDIR reached through a symlink,
    so path-alias handling macOS needs is exercised on Linux too."""
    if platform.system() != "Linux":
        return None
    scratch = Path(tempfile.mkdtemp(prefix="tcl.", dir=os.environ.get("TMPDIR"))).resolve()
    stack.append(lambda: shutil.rmtree(scratch, ignore_errors=True))
    (scratch / "real").mkdir()
    (scratch / "alias").symlink_to(scratch / "real")
    return str(scratch / "alias")


class Partition:
    def __init__(self, index, names_file, log_path):
        self.name = f"p{index}"
        self.names_file = names_file
        self.log_path = log_path
        self.running = {}  # top-level test -> monotonic start
        self.lock = threading.Lock()
        self.proc = None
        self.reader = None
        self.status = None
        self.watchdog_fired = None
        self.started = None
        self.exited_at = None
        self.notes = []  # runner-side problems, printed in the report

    def start(self, binary, run_args, env, cover_dir):
        args = [str(binary), "-test.v=test2json", "-test.paniconexit0", *run_args]
        if cover_dir:
            covdata = cover_dir / f"{self.name}.covdata"
            covdata.mkdir(parents=True, exist_ok=True)
            args += [f"-test.gocoverdir={covdata}", f"-test.coverprofile={cover_dir / (self.name + '.cover')}"]
        env = dict(env, TCLAUDE_AGENTD_TEST_PARTITION=str(self.names_file))
        self.started = time.monotonic()
        self.proc = subprocess.Popen(args, cwd=ROOT / PKG_DIR, env=env,
                                     stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        self.conv = subprocess.Popen([os.environ.get("GO", "go"), "tool", "test2json", "-t", "-p", PKG],
                                     stdin=self.proc.stdout, stdout=subprocess.PIPE,
                                     encoding="utf-8", errors="replace")
        self.proc.stdout.close()  # test2json owns the read end now
        self.reader = threading.Thread(target=self._read, daemon=True)
        self.reader.start()

    def _read(self):
        # Keep draining test2json whatever happens here: a reader that stopped
        # would fill the pipe and block test2json, then the test binary.
        log = None
        try:
            log = open(self.log_path, "w", encoding="utf-8")
        except OSError as error:
            self.notes.append(f"runner could not write {self.log_path}: {error}")
        for line in self.conv.stdout:
            try:
                if log:
                    log.write(line)
                self._track(line)
            except Exception as error:  # noqa: BLE001 - must never stop draining
                if log:
                    self.notes.append(f"runner stopped logging this process: {error!r}")
                    log = None
        if log:
            log.close()

    def _track(self, line):
        try:
            event = json.loads(line)
        except ValueError:
            return
        name = event.get("Test", "") if isinstance(event, dict) else ""
        if not name or "/" in name:
            return
        action = event.get("Action")
        with self.lock:
            if action in ("run", "cont"):
                self.running[name] = time.monotonic()
            elif action in ("pass", "fail", "skip", "pause"):
                self.running.pop(name, None)

    def check_watchdog(self, limit):
        if self.watchdog_fired or self.proc.poll() is not None:
            return
        now = time.monotonic()
        with self.lock:
            overdue = [(name, now - start) for name, start in self.running.items() if now - start > limit]
        if not overdue:
            return
        name, elapsed = max(overdue, key=lambda item: item[1])
        self.watchdog_fired = (name, elapsed, now)
        print(f"::error title=agentd test hang::{name} has run {elapsed:.0f}s (limit {limit:.0f}s) "
              f"in {self.name}; sending SIGQUIT for a goroutine dump", flush=True)
        self.proc.send_signal(signal.SIGQUIT)

    def check_deadline(self, deadline):
        """Stop a process that outlives its whole budget: a hang outside any
        top-level test (TestMain setup or teardown) has no test to blame."""
        if self.watchdog_fired or self.proc.poll() is not None:
            return
        now = time.monotonic()
        if now - self.started <= deadline:
            return
        with self.lock:
            running = sorted(self.running)
        where = ", ".join(running) if running else "outside any test (TestMain setup or teardown)"
        self.watchdog_fired = (where, now - self.started, now)
        print(f"::error title=agentd test hang::{self.name} has run {now - self.started:.0f}s, past its "
              f"{deadline:.0f}s budget, in {where}; sending SIGQUIT for a goroutine dump", flush=True)
        self.proc.send_signal(signal.SIGQUIT)

    def check_kill(self):
        if self.watchdog_fired and self.proc.poll() is None and time.monotonic() - self.watchdog_fired[2] > KILL_GRACE:
            self.proc.kill()

    def check_drain(self):
        if self.proc.poll() is None or not self.reader.is_alive():
            return
        if self.exited_at is None:
            self.exited_at = time.monotonic()
        elif time.monotonic() - self.exited_at > DRAIN_GRACE and self.conv.poll() is None:
            self.notes.append(f"the test binary exited but its output stayed open for {DRAIN_GRACE:.0f}s: "
                              "a process a test started still holds it; stopped reading")
            self.conv.kill()

    def finished(self):
        return self.proc.poll() is not None and not self.reader.is_alive()

    def wait(self):
        self.status = self.proc.wait()
        self.conv.wait()
        self.reader.join()


def build(go, build_args, logs, cover):
    binary = logs / "agentd.test"
    cmd = [go, "test", "-c", "-o", str(binary), *build_args]
    if cover:
        cmd.append("-cover")
    cmd.append("./" + PKG_DIR)
    try:
        result = subprocess.run(cmd, cwd=ROOT, text=True, capture_output=True, timeout=1800)
    except subprocess.TimeoutExpired:
        raise SystemExit("agentd-tests: building the test binary took over 30 minutes")
    if result.returncode != 0:
        print(result.stdout + result.stderr, end="")
        print("agentd-tests: building the test binary failed", file=sys.stderr)
        raise SystemExit(1)
    return binary


def list_tests(binary, env):
    try:
        result = subprocess.run([str(binary), "-test.list", ".*"], cwd=ROOT / PKG_DIR, env=env,
                                text=True, capture_output=True, timeout=300)
    except subprocess.TimeoutExpired:
        raise SystemExit("agentd-tests: listing the tests took over 5 minutes")
    names = sorted(line.strip() for line in result.stdout.splitlines() if TEST_NAME.match(line.strip()))
    if result.returncode != 0 or not names:
        print(result.stdout + result.stderr, end="")
        print("agentd-tests: listing the tests failed", file=sys.stderr)
        raise SystemExit(1)
    return names


def report(part):
    """Print one partition's result; return (ok, durations)."""
    output, failed, raw, summaries, durations = {}, [], [], [], {}
    started, ended = set(), set()
    for line in Path(part.log_path).read_text(errors="replace").splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            raw.append(line)
            continue
        if not isinstance(event, dict) or "Action" not in event:
            raw.append(line)
            continue
        name, action = event.get("Test", ""), event.get("Action")
        if action == "output":
            output.setdefault(name, []).append(event.get("Output", ""))
        top = name and "/" not in name
        if top and action == "run":
            started.add(name)
        if top and action in ("pass", "fail", "skip"):
            ended.add(name)
            durations[name] = event.get("Elapsed", 0)
        if action == "fail" and name:
            failed.append(name)
        if action in ("pass", "fail") and not name:
            summaries.append(f"{action.upper()} ({event.get('Elapsed', 0):.3f}s)")
    unfinished = sorted(started - ended)
    print(f"=== agentd {part.name}: exit {part.status} ===")
    for summary in summaries:
        print(summary)
    ok = part.status == 0 and bool(summaries) and not part.notes
    for note in part.notes:
        print(f"Runner: {note}")
    if part.status or part.notes:
        if part.watchdog_fired:
            name, elapsed, _ = part.watchdog_fired
            print(f"Watchdog: {name} ran {elapsed:.0f}s without finishing; the goroutine dump follows its output.")
        for name in failed:
            print(f"--- failed test: {name} ---")
            print("".join(output.get(name, [])), end="")
        for name in unfinished:
            print(f"--- still running when the process ended: {name} ---")
            print("".join(output.get(name, [])), end="")
        if not failed and not unfinished:
            print("No named test failure: build, setup, or process failed.")
        print("".join(output.get("", [])), end="")
        for line in raw:
            print(line)
        named = failed + [n for n in unfinished if n not in failed]
        print(f"Failing tests ({part.name}): " + (", ".join(named) or "(see build/setup output)"))
    elif not summaries:
        # A test process returning success without a package result is suspicious.
        print(f"{part.name}: missing package result", file=sys.stderr)
    return ok, durations


def summarize(durations, walls):
    slowest = sorted(durations.items(), key=lambda item: -item[1])[:20]
    lines = ["Partition wall times: " + ", ".join(f"{n} {w:.0f}s" for n, w in walls),
             f"Tests: {len(durations)}, summed test time {sum(durations.values()):.0f}s",
             "Slowest tests (seconds):"]
    lines += [f"  {seconds:7.2f}  {name}" for name, seconds in slowest]
    print("\n".join(lines))
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a") as out:
            out.write("### agentd tests\n\n" + lines[0] + "  \n" + lines[1] + "\n\n| seconds | test |\n|---:|---|\n")
            out.writelines(f"| {seconds:.2f} | {name} |\n" for name, seconds in slowest)


def main(argv):
    build_args, run_args, timeout = parse_args(argv)
    go = os.environ.get("GO", "go")
    cleanups = []
    try:
        if os.environ.get("TCLAUDE_AGENTD_TEST_LOG_DIR"):
            logs = Path(os.environ["TCLAUDE_AGENTD_TEST_LOG_DIR"]).resolve()
            logs.mkdir(parents=True, exist_ok=True)
        else:
            logs = Path(tempfile.mkdtemp(prefix="tcl-agentd-tests."))
            cleanups.append(lambda: shutil.rmtree(logs, ignore_errors=True))
        cover = os.environ.get("TCLAUDE_AGENTD_TEST_COVER") == "1"
        procs = int(os.environ.get("TCLAUDE_AGENTD_TEST_PROCS") or default_procs())
        limit = float(os.environ.get("TCLAUDE_AGENTD_TEST_WATCHDOG") or DEFAULT_WATCHDOG)
        if "-race" in build_args:
            limit *= 3  # -race roughly triples wall time
        deadline = timeout + PROCESS_GRACE

        env = dict(os.environ)
        env.pop("TCLAUDE_AGENTD_TEST_PARTITION", None)
        tmpdir = alias_tmpdir(cleanups)
        if tmpdir:
            env["TMPDIR"] = tmpdir

        binary = build(go, build_args, logs, cover)
        names = list_tests(binary, env)
        procs = max(1, min(procs, len(names)))
        parts = []
        for index in range(procs):
            names_file = logs / f"p{index + 1}.names"
            names_file.write_text("\n".join(names[index::procs]) + "\n")
            parts.append(Partition(index + 1, names_file, logs / f"p{index + 1}.json"))
        print(f"agentd-tests: {len(names)} tests in {procs} processes; per-test watchdog {limit:.0f}s", flush=True)

        began = time.monotonic()
        walls = {}
        for part in parts:
            part.start(binary, run_args, env, logs if cover else None)
        while len(walls) < len(parts):
            for part in parts:
                if part.name in walls:
                    continue
                if part.finished():
                    walls[part.name] = time.monotonic() - began
                    continue
                part.check_watchdog(limit)
                part.check_deadline(deadline)
                part.check_kill()
                part.check_drain()
            time.sleep(0.5)
        for part in parts:
            part.wait()

        # Report only after every process completes: assertions and failing
        # test names are grouped by partition, never interleaved.
        all_ok, durations = True, {}
        for part in parts:
            ok, part_durations = report(part)
            all_ok = all_ok and ok
            durations.update(part_durations)
        summarize(durations, [(part.name, walls[part.name]) for part in parts])
        return 0 if all_ok else 1
    finally:
        for cleanup in reversed(cleanups):
            cleanup()


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
