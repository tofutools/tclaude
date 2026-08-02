package agentd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// consoleTmuxRec records the tmux argv the console launcher issues and scripts
// the two answers it branches on: whether the console's session is still there
// (`has-session`) and what pid is in its pane (`display-message`).
type consoleTmuxRec struct {
	calls [][]string
	// aliveAnswers is consumed one per has-session; an exhausted queue answers
	// "gone", which is the ordinary end state.
	aliveAnswers []bool
	panePID      string
	failCreate   bool
	// attachStderr is what the stand-in `attach-session` writes to stderr
	// before exiting 1 — how tmux reports "no sessions" under a console that
	// died before it could write its own error file.
	attachStderr string
	// consoleError is written to the error file the launch script names, as the
	// inner daemon would on a startup failure. Applied when attach runs, since
	// that is when a real console would already have failed.
	consoleError string
	// launchScript is the path new-session was told to run.
	launchScript string
}

func (r *consoleTmuxRec) Command(args ...string) *exec.Cmd {
	r.calls = append(r.calls, append([]string(nil), args...))
	switch {
	case slices.Contains(args, "new-session"):
		if r.failCreate {
			return exec.Command("false")
		}
		r.launchScript = args[len(args)-1]
		return exec.Command("true")
	case slices.Contains(args, "attach-session"):
		if r.consoleError != "" {
			_ = os.WriteFile(r.consoleErrorFilePath(), []byte(r.consoleError), 0o600)
		}
		if r.attachStderr != "" {
			// tmux's own shape for a failed attach: the message on stderr,
			// exit 1. The script is a compile-time constant; only the message
			// the test chose is interpolated, through argv rather than the
			// script body.
			return exec.Command("sh", "-c", `printf '%s\n' "$1" >&2; exit 1`, "sh", r.attachStderr)
		}
		return exec.Command("true")
	case slices.Contains(args, "has-session"):
		alive := false
		if len(r.aliveAnswers) > 0 {
			alive, r.aliveAnswers = r.aliveAnswers[0], r.aliveAnswers[1:]
		}
		if alive {
			return exec.Command("true")
		}
		return exec.Command("false")
	case slices.Contains(args, "display-message"):
		if r.panePID == "" {
			return exec.Command("false")
		}
		return exec.Command("echo", r.panePID)
	}
	return exec.Command("true")
}

// consoleErrorFilePath digs the error-file path back out of the launch script
// the launcher wrote, which is the only place it exists — the launcher picks it
// itself and never returns it.
func (r *consoleTmuxRec) consoleErrorFilePath() string {
	raw, err := os.ReadFile(r.launchScript)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(raw), "; ") {
		prefix := "export " + tuiConsoleErrorFileEnv + "="
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			return strings.Trim(after, "'")
		}
	}
	return ""
}

func (r *consoleTmuxRec) ListSessions() (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

func (r *consoleTmuxRec) call(subcommand string) ([]string, bool) {
	for _, call := range r.calls {
		if slices.Contains(call, subcommand) {
			return call, true
		}
	}
	return nil, false
}

func (r *consoleTmuxRec) issued(subcommand string) bool {
	_, ok := r.call(subcommand)
	return ok
}

// installConsoleTmuxRec swaps the tmux boundary for the recorder and clears
// TMUX so the launcher's own "am I already in tmux" test answers from the
// environment rather than through the recorder.
func installConsoleTmuxRec(t *testing.T, rec *consoleTmuxRec) *consoleTmuxRec {
	t.Helper()
	t.Setenv("TMUX", "")
	prev := clcommon.Default
	clcommon.Default = rec
	t.Cleanup(func() { clcommon.Default = prev })
	return rec
}

// tuiConsoleStubAncestry pins the harness-ancestry probe. Tests must set it
// either way: the test binary's own ancestry is whatever ran `go test`, which
// on a developer's machine is quite often a coding agent.
func tuiConsoleStubAncestry(t *testing.T, hasHarnessAncestor bool) {
	t.Helper()
	prev := tuiConsoleHasHarnessAncestor
	tuiConsoleHasHarnessAncestor = func() bool { return hasHarnessAncestor }
	t.Cleanup(func() { tuiConsoleHasHarnessAncestor = prev })
}

// TestTUIConsoleRelaunchRequested pins when `serve --tui` hands itself to a
// console in a tmux session of its own. The two "already inside tmux" cases are
// the same rule read twice: an operator's own tmux already gives the console a
// session, and the console the launcher started must not start another.
func TestTUIConsoleRelaunchRequested(t *testing.T) {
	cases := map[string]struct {
		params     serveParams
		tmux       string
		consoleEnv string
		want       bool
	}{
		"no console at all":      {serveParams{}, "", "", false},
		"tui from a plain shell": {serveParams{TUI: true}, "", "", true},
		"tui inside the operator's own tmux": {
			serveParams{TUI: true}, "/tmp/tmux-1000/default,123,0", "", false},
		"the console the launcher started": {
			serveParams{TUI: true}, "", "tclaude-console", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TMUX", tc.tmux)
			t.Setenv(tuiConsoleSessionEnv, tc.consoleEnv)
			if got := tuiConsoleRelaunchRequested(&tc.params); got != tc.want {
				t.Errorf("tuiConsoleRelaunchRequested(%+v) = %v, want %v", tc.params, got, tc.want)
			}
		})
	}
}

// TestTUIConsoleUnavailable_ExternalTmuxRuntime keeps the console off a
// delegated tmux server: it is a separate, longer-lived unit, so putting the
// daemon's own session on it would run it inside the delegated cgroup and hand
// its lifetime to something meant to outlive it.
func TestTUIConsoleUnavailable_ExternalTmuxRuntime(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tuiConsoleStubAncestry(t, false)
	tuiConsoleStubTerminal(t, true)
	dir := "/sys/fs/cgroup/system.slice/tclaude-tmux.service"

	reason := tuiConsoleUnavailable(&serveParams{TUI: true, ResourceDelegationDir: dir})

	if !strings.Contains(reason, dir) {
		t.Fatalf("tuiConsoleUnavailable = %q, want it to name the external runtime %q", reason, dir)
	}
}

// TestTUIConsoleUnavailable_NoTerminal covers the degradation that matters for
// scripted launches: tmux cannot attach a session to a pipe, so a console
// without a terminal keeps the one it has instead of failing startup.
func TestTUIConsoleUnavailable_NoTerminal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tuiConsoleStubAncestry(t, false)
	tuiConsoleStubTerminal(t, false)

	if reason := tuiConsoleUnavailable(&serveParams{TUI: true}); reason == "" {
		t.Fatal("a console with no terminal must report a reason, not claim a tmux session")
	}
}

// TestRunTUIConsoleInTmux_DegradesInsteadOfFailing is the contract around every
// reason above: the launcher declines and the caller runs the console in this
// terminal, exactly as it did before consoles had sessions of their own.
func TestRunTUIConsoleInTmux_DegradesInsteadOfFailing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rec := installConsoleTmuxRec(t, &consoleTmuxRec{})
	tuiConsoleStubAncestry(t, false)
	tuiConsoleStubTerminal(t, false)

	handled, err := runTUIConsoleInTmux(&serveParams{TUI: true})

	if handled || err != nil {
		t.Fatalf("runTUIConsoleInTmux = (%v, %v), want (false, nil) so the console runs in place", handled, err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("a declined relaunch must touch tmux not at all, got %v", rec.calls)
	}
}

// TestTUIConsoleUnavailable_HarnessAncestorKeepsItsClassification is the
// security guard, and the reason the ancestry check does not simply trust TMUX.
// The daemon classifies its own console by walking the process tree — a harness
// ancestor beats an operator token — and relaunching reparents the daemon under
// the tmux server, erasing that ancestor. `env -u TMUX tclaude agentd serve
// --tui` from an agent's pane would otherwise turn an agent-class console into
// an operator one.
func TestTUIConsoleUnavailable_HarnessAncestorKeepsItsClassification(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMUX", "")
	tuiConsoleStubAncestry(t, true)
	tuiConsoleStubTerminal(t, true)

	reason := tuiConsoleUnavailable(&serveParams{TUI: true})

	if !strings.Contains(reason, "coding harness") {
		t.Fatalf("tuiConsoleUnavailable = %q, want it to decline on the harness ancestor", reason)
	}
}

// TestRunTUIConsoleInTmux_HappyPath walks the whole launcher once: create the
// session, hand it the terminal, and stop it afterwards. The stop is
// unconditional by design, so a console that quit on its own must still be
// probed for and found gone.
func TestRunTUIConsoleInTmux_HappyPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rec := installConsoleTmuxRec(t, &consoleTmuxRec{})
	tuiConsoleStubAncestry(t, false)
	tuiConsoleStubTerminal(t, true)

	handled, err := runTUIConsoleInTmux(&serveParams{TUI: true})

	if !handled || err != nil {
		t.Fatalf("runTUIConsoleInTmux = (%v, %v), want (true, nil)", handled, err)
	}
	for _, want := range []string{"new-session", "attach-session", "has-session"} {
		if !rec.issued(want) {
			t.Errorf("launcher never issued %s; calls were %v", want, rec.calls)
		}
	}
	if rec.issued("kill-session") {
		t.Fatalf("a console that quit on its own must not be killed, got %v", rec.calls)
	}
}

// TestRunTUIConsoleInTmux_ReportsTheConsolesOwnError is why the error file
// exists. The console's pane is destroyed moments after it fails, so its
// message has to be handed back to the launcher, which still has a terminal to
// print it on — and it outranks whatever tmux made of attaching to a session
// that was already gone.
func TestRunTUIConsoleInTmux_ReportsTheConsolesOwnError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installConsoleTmuxRec(t, &consoleTmuxRec{
		consoleError: "another agentd already owns /home/x/.tclaude/data",
		attachStderr: "no sessions",
	})
	tuiConsoleStubAncestry(t, false)
	tuiConsoleStubTerminal(t, true)

	handled, err := runTUIConsoleInTmux(&serveParams{TUI: true})

	if !handled || err == nil {
		t.Fatalf("runTUIConsoleInTmux = (%v, %v), want the console's failure reported", handled, err)
	}
	if !strings.Contains(err.Error(), "another agentd already owns") {
		t.Fatalf("error = %q, want the console's own message rather than tmux's", err)
	}
}

// TestRunTUIConsoleInTmux_AFailedAttachIsNotASilentSuccess covers the case with
// no error file to fall back on: the pane died before the daemon could write
// one, so all that is left is tmux's complaint. tmux exits 1 on a clean detach
// too, so without reading stderr this path would return success from a run that
// never started a console.
func TestRunTUIConsoleInTmux_AFailedAttachIsNotASilentSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installConsoleTmuxRec(t, &consoleTmuxRec{attachStderr: "can't find session: tclaude-console"})
	tuiConsoleStubAncestry(t, false)
	tuiConsoleStubTerminal(t, true)

	handled, err := runTUIConsoleInTmux(&serveParams{TUI: true})

	if !handled || err == nil {
		t.Fatalf("runTUIConsoleInTmux = (%v, %v), want a failed attach reported", handled, err)
	}
	if !strings.Contains(err.Error(), "can't find session") {
		t.Fatalf("error = %q, want tmux's own complaint", err)
	}
}

// TestAttachToTUIConsoleSession_PlainDetachIsNotAFailure is the other side of
// that rule. A detach and a dead session both exit 1; only one of them says
// anything on stderr, and the operator quitting the console must not be
// reported as an error.
func TestAttachToTUIConsoleSession_PlainDetachIsNotAFailure(t *testing.T) {
	installConsoleTmuxRec(t, &consoleTmuxRec{attachStderr: ""})

	if err := attachToTUIConsoleSession("tclaude-console"); err != nil {
		t.Fatalf("attachToTUIConsoleSession = %v, want a clean detach to be no error", err)
	}
}

// TestTrapTUIConsoleSignals_StopsTheConsole is the foreground contract under a
// launcher that is killed rather than detached from — an ssh drop, a closed
// terminal window. Without it the console session and the daemon in it would
// survive with no client, invisible, and would then block the next `serve
// --tui` on the singleton lock.
func TestTrapTUIConsoleSignals_StopsTheConsole(t *testing.T) {
	stopped := make(chan struct{})
	stopTrapping := trapTUIConsoleSignals(func() { close(stopped) })
	defer stopTrapping()

	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("signal this process: %v", err)
	}

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("a signalled launcher must stop the console it started")
	}
}

// TestSweepStaleTUIConsoleErrorFiles keeps a launcher that never got to clean
// up — a SIGKILL, a lost host — from accumulating error files forever, without
// touching one a live launcher may still be waiting on.
func TestSweepStaleTUIConsoleErrorFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	stale := filepath.Join(dir, "error-old")
	fresh := filepath.Join(dir, "error-new")
	unrelated := filepath.Join(dir, "something-else")
	for _, p := range []string{stale, fresh, unrelated} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	old := now.Add(-2 * tuiConsoleErrorFileMaxAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("age the stale file: %v", err)
	}
	if err := os.Chtimes(unrelated, old, old); err != nil {
		t.Fatalf("age the unrelated file: %v", err)
	}

	sweepStaleTUIConsoleErrorFiles(dir, now)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale error file survived the sweep (stat err %v)", err)
	}
	for _, p := range []string{fresh, unrelated} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("sweep removed %s, which it had no business touching: %v", p, err)
		}
	}
}

// TestStartTUIConsoleSession_LaunchShape pins what actually reaches tmux: a
// detached session under the console's own name, and a launch script rather
// than a bare argv — the script is what re-exports THIS process's environment,
// since tmux hands a new pane the server's environment, which on a server that
// was already up can be from an entirely different shell.
func TestStartTUIConsoleSession_LaunchShape(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rec := installConsoleTmuxRec(t, &consoleTmuxRec{})
	t.Setenv("TCLAUDE_CONSOLE_LAUNCH_PROBE", "carried-through")
	stubArgs := []string{"tclaude", "agentd", "serve", "--tui", "--dashboard-port", "8321"}
	prevArgs := os.Args
	os.Args = stubArgs
	t.Cleanup(func() { os.Args = prevArgs })

	if err := startTUIConsoleSession("tclaude-console", "/usr/local/bin/tclaude",
		"/tmp/err"); err != nil {
		t.Fatalf("startTUIConsoleSession: %v", err)
	}

	call, ok := rec.call("new-session")
	if !ok {
		t.Fatalf("no new-session issued, got %v", rec.calls)
	}
	if !slices.Contains(call, "-d") || !slices.Contains(call, "tclaude-console") {
		t.Fatalf("new-session argv = %v, want a detached session named tclaude-console", call)
	}
	scriptPath := call[len(call)-1]
	if call[len(call)-2] != "sh" || !strings.Contains(scriptPath, "launch") {
		t.Fatalf("new-session argv = %v, want it to end in `sh <launch script>`", call)
	}

	// The pane's script is self-deleting, so read it before it runs — nothing
	// ran it here, so it is still on disk.
	raw, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read launch script: %v", err)
	}
	script := string(raw)
	for _, want := range []string{
		"export " + tuiConsoleSessionEnv + "=tclaude-console",
		"export " + tuiConsoleErrorFileEnv + "=/tmp/err",
		"export TCLAUDE_CONSOLE_LAUNCH_PROBE=carried-through",
		// `exec` so #{pane_pid} is the daemon itself and the shutdown path's
		// SIGTERM is not swallowed by a surviving `sh` wrapper.
		"exec " + clcommon.ShellQuoteArg("/usr/local/bin/tclaude"),
		// The invocation itself has to survive into the pane, --tui included:
		// the console inside the session is a `serve --tui` in every respect
		// except that it finds itself inside tmux.
		" agentd serve --tui --dashboard-port 8321",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("launch script is missing %q\n---\n%s", want, script)
		}
	}
}

// TestStartTUIConsoleSession_FailedCreateIsFatal is the one place the console
// does NOT degrade to this terminal: the decision to relaunch has been made,
// and a tmux that refuses to create the session ("bad session name", "can't
// create socket") is a real failure the operator has to see rather than a host
// capability to route around.
func TestStartTUIConsoleSession_FailedCreateIsFatal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installConsoleTmuxRec(t, &consoleTmuxRec{failCreate: true})

	err := startTUIConsoleSession("tclaude-console", "/usr/local/bin/tclaude", "/tmp/err")

	if err == nil || !strings.Contains(err.Error(), "tclaude-console") {
		t.Fatalf("startTUIConsoleSession = %v, want an error naming the session it could not create", err)
	}
}

// TestStopTUIConsoleSession_QuitLeavesNothingToDo is the ordinary exit: the
// operator quit the console, its session is already gone, and the launcher must
// not go looking for something to kill.
func TestStopTUIConsoleSession_QuitLeavesNothingToDo(t *testing.T) {
	rec := installConsoleTmuxRec(t, &consoleTmuxRec{})

	stopTUIConsoleSession("tclaude-console")

	if rec.issued("kill-session") {
		t.Fatalf("a console that already quit must not be killed, got %v", rec.calls)
	}
	if len(rec.calls) != 1 || !slices.Contains(rec.calls[0], "has-session") {
		t.Fatalf("tmux calls = %v, want the liveness probe and nothing else", rec.calls)
	}
}

// TestStopTUIConsoleSession_DetachStopsTheDaemonGracefully is the detach path.
// `serve --tui` is a foreground process whose face is the console, so losing
// sight of it ends the run — but through SIGTERM to the pane's own pid, so the
// daemon drains HTTP, flushes checkpoints and releases its singleton lock
// instead of being torn out from under itself by kill-session.
func TestStopTUIConsoleSession_DetachStopsTheDaemonGracefully(t *testing.T) {
	// A real process stands in for the console daemon, so the SIGTERM is a
	// SIGTERM and not a mock: the test asserts the signal landed by waiting for
	// it to die of it. The alive queue says the session is up for the initial
	// probe and gone on the first poll after the signal.
	victim := exec.Command("sleep", "60")
	if err := victim.Start(); err != nil {
		t.Fatalf("start stand-in console process: %v", err)
	}
	t.Cleanup(func() { _ = victim.Process.Kill(); _, _ = victim.Process.Wait() })
	rec := installConsoleTmuxRec(t, &consoleTmuxRec{
		aliveAnswers: []bool{true, false},
		panePID:      strconv.Itoa(victim.Process.Pid),
	})

	stopTUIConsoleSession("tclaude-console")

	if !rec.issued("display-message") {
		t.Fatalf("a live console must be signalled by pane pid, got %v", rec.calls)
	}
	if rec.issued("kill-session") {
		t.Fatalf("a console that shut down on SIGTERM must not also be killed, got %v", rec.calls)
	}
	if err := victim.Wait(); err == nil || !strings.Contains(err.Error(), "terminated") {
		t.Fatalf("stand-in console exited with %v, want death by SIGTERM", err)
	}
}

// TestStopTUIConsoleSession_KillsAConsoleThatWillNotGo is the backstop. A pane
// whose pid tmux cannot report leaves nothing to signal, and the operator's
// terminal must not be held by a console that cannot be reached.
func TestStopTUIConsoleSession_KillsAConsoleThatWillNotGo(t *testing.T) {
	rec := installConsoleTmuxRec(t, &consoleTmuxRec{aliveAnswers: []bool{true}})

	stopTUIConsoleSession("tclaude-console")

	if !rec.issued("kill-session") {
		t.Fatalf("an unreachable console must be killed, got %v", rec.calls)
	}
}

// TestTUIConsoleStartupErrorRoundTrip covers the one channel a console has for
// a startup failure. Its pane is destroyed moments after runServe returns, so
// without this the operator's "another agentd already owns …" would be drawn
// into a window that no longer exists by the time they could read it.
func TestTUIConsoleStartupErrorRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console-error")
	t.Setenv(tuiConsoleErrorFileEnv, path)

	recordTUIConsoleStartupError(fmt.Errorf("another agentd already owns %s", "/home/x/.tclaude/data"))

	if got := readTUIConsoleError(path); got != "another agentd already owns /home/x/.tclaude/data" {
		t.Fatalf("readTUIConsoleError = %q, want the console's own error text", got)
	}
}

// TestRecordTUIConsoleStartupError_OnlyForALaunchedConsole keeps the daemon
// that nobody launched — a plain `agentd serve` — from writing anywhere. The
// path it WOULD have used is created first, so the assertion fails if the
// unset-env guard is dropped rather than passing for want of a target.
func TestRecordTUIConsoleStartupError_OnlyForALaunchedConsole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console-error")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("seed the error file: %v", err)
	}
	t.Setenv(tuiConsoleErrorFileEnv, "")

	recordTUIConsoleStartupError(errors.New("boom"))

	if got := readTUIConsoleError(path); got != "" {
		t.Fatalf("an unlaunched daemon wrote %q, want nothing", got)
	}
}

// tuiConsoleStubTerminal pins the terminal probe for a test process that has no
// terminal of its own.
func tuiConsoleStubTerminal(t *testing.T, isTerminal bool) {
	t.Helper()
	prev := tuiConsoleStdioIsTerminal
	tuiConsoleStdioIsTerminal = func() bool { return isTerminal }
	t.Cleanup(func() { tuiConsoleStdioIsTerminal = prev })
}
