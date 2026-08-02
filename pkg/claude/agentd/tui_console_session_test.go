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
	"testing"

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
}

func (r *consoleTmuxRec) Command(args ...string) *exec.Cmd {
	r.calls = append(r.calls, append([]string(nil), args...))
	switch {
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
	case r.failCreate && slices.Contains(args, "new-session"):
		return exec.Command("false")
	}
	return exec.Command("true")
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
		"no console at all":       {serveParams{}, "", "", false},
		"tui from a plain shell":  {serveParams{TUI: true}, "", "", true},
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
// delegated tmux server for the same reason startTUITmuxServer keeps its hands
// off one: that server is a separate, longer-lived unit, so putting the daemon's
// own session on it would run it inside the delegated cgroup and hand its
// lifetime to something meant to outlive it.
func TestTUIConsoleUnavailable_ExternalTmuxRuntime(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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
	tuiConsoleStubTerminal(t, false)

	handled, err := runTUIConsoleInTmux(&serveParams{TUI: true})

	if handled || err != nil {
		t.Fatalf("runTUIConsoleInTmux = (%v, %v), want (false, nil) so the console runs in place", handled, err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("a declined relaunch must touch tmux not at all, got %v", rec.calls)
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

	if err := startTUIConsoleSession("tclaude-console", "/usr/local/bin/tclaude",
		"/tmp/err", true); err != nil {
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
		"export " + tuiConsoleOwnsServerEnv + "=1",
		"export TCLAUDE_CONSOLE_LAUNCH_PROBE=carried-through",
		// `exec` so #{pane_pid} is the daemon itself and the shutdown path's
		// SIGTERM is not swallowed by a surviving `sh` wrapper.
		"exec " + clcommon.ShellQuoteArg("/usr/local/bin/tclaude"),
	} {
		if !strings.Contains(script, want) {
			t.Errorf("launch script is missing %q\n---\n%s", want, script)
		}
	}
}

// TestStartTUIConsoleSession_OwnershipIsNotAsserted keeps the ownership
// handshake honest in the other direction: a launcher that did not take the
// tmux server must not leave a console believing its quit will take the
// server's other sessions down.
func TestStartTUIConsoleSession_OwnershipIsNotAsserted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rec := installConsoleTmuxRec(t, &consoleTmuxRec{})

	if err := startTUIConsoleSession("tclaude-console", "/usr/local/bin/tclaude",
		"/tmp/err", false); err != nil {
		t.Fatalf("startTUIConsoleSession: %v", err)
	}

	call, _ := rec.call("new-session")
	raw, err := os.ReadFile(call[len(call)-1])
	if err != nil {
		t.Fatalf("read launch script: %v", err)
	}
	if strings.Contains(string(raw), tuiConsoleOwnsServerEnv) {
		t.Fatalf("launch script asserts tmux-server ownership nobody took:\n%s", raw)
	}
}

// TestStartTUIConsoleSession_FailedCreateIsFatal is the one place the console
// does NOT degrade to this terminal: the decision to relaunch has been made,
// the launcher may already have taken the tmux server, and a tmux that refuses
// to create the session ("bad session name", "can't create socket") is a real
// failure the operator has to see rather than a host capability to route
// around.
func TestStartTUIConsoleSession_FailedCreateIsFatal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installConsoleTmuxRec(t, &consoleTmuxRec{failCreate: true})

	err := startTUIConsoleSession("tclaude-console", "/usr/local/bin/tclaude", "/tmp/err", false)

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
// that nobody launched — a plain `agentd serve` — from writing anywhere.
func TestRecordTUIConsoleStartupError_OnlyForALaunchedConsole(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(tuiConsoleErrorFileEnv, "")

	recordTUIConsoleStartupError(errors.New("boom"))

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("an unlaunched daemon wrote %v (err %v), want nothing", entries, err)
	}
}

// TestTUIConsoleOwnsTmuxServer pins the inherited half of the ownership
// handshake: only the launcher's own "1" counts, so a console reading a
// leftover or malformed value never tells the operator that quitting will take
// their agent panes with it.
func TestTUIConsoleOwnsTmuxServer(t *testing.T) {
	for value, want := range map[string]bool{"1": true, "": false, "0": false, "true": false} {
		t.Setenv(tuiConsoleOwnsServerEnv, value)
		if got := tuiConsoleOwnsTmuxServer(); got != want {
			t.Errorf("tuiConsoleOwnsTmuxServer() with %q = %v, want %v", value, got, want)
		}
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

