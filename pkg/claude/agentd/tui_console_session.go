package agentd

// The `tclaude agentd serve --tui` console runs in a tmux session of its own on
// the `-L tclaude` server instead of straight in the operator's terminal, and
// that session goes away again when the console does.
//
// What it buys: `enter` on an agent becomes tmux's own `switch-client` rather
// than an `attach-session` that takes the terminal away from bubbletea. The
// console keeps running in its own window, so the operator moves between it and
// an agent with tmux keys and comes back to a console that never stopped
// drawing. realTUIAttachToPane already branches on exactly that — it just never
// saw the tmux side of the branch under a normal `serve --tui`.
//
// A running bubbletea program owns the terminal it started on and cannot move
// itself into tmux, so the process the operator launched becomes a LAUNCHER: it
// creates the session, runs a second `tclaude agentd serve --tui` inside it,
// hands this terminal to it, and stops the session again on the way out. That
// inner daemon is the real one — it holds the singleton lock, the database and
// the sockets; the launcher holds nothing but the tmux client.
//
// The recursion guard is the ordinary "am I inside tmux" test: tmux sets TMUX
// for the console's pane, so the inner run takes the in-place path, which is
// also what a `serve --tui` started from inside the operator's own tmux does.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// The launcher → console handshake. Both share clcommon.TUIConsoleEnvPrefix,
// which BuildEnvExports strips from every pane the console launches: they
// describe one specific console process, and an agent pane that inherited them
// would answer for a daemon it is not (and, in the error file's case, overwrite
// the launcher's only channel for a startup failure).
const (
	// tuiConsoleSessionEnv names the console's own tmux session. Its presence
	// is what tells the inner daemon it was started by a launcher rather than
	// by an operator who happened to be sitting in tmux.
	tuiConsoleSessionEnv = clcommon.TUIConsoleEnvPrefix + "SESSION"
	// tuiConsoleErrorFileEnv is where the inner daemon writes a startup failure
	// so the launcher can print it on the real terminal. Without it the message
	// would be drawn into a pane that is destroyed a moment later, and a
	// "another agentd already owns …" would simply vanish.
	tuiConsoleErrorFileEnv = clcommon.TUIConsoleEnvPrefix + "ERROR_FILE"
)

// tuiConsoleSessionBase is the tmux name the console asks for; a live one is
// suffixed rather than reused, so a second console never lands on the first
// one's session.
const tuiConsoleSessionBase = "tclaude-console"

// How long a detached-from console gets to shut down on its own after SIGTERM
// before the session is killed outright. The daemon's own drain is two bounded
// 3s windows plus the tray teardown, so this is comfortably above a healthy
// exit and still short enough that a wedged one does not hold the operator's
// terminal.
const (
	tuiConsoleStopGrace = 15 * time.Second
	tuiConsoleStopPoll  = 100 * time.Millisecond
)

// Where the launcher keeps the console's startup-error file, how long a
// stranded one survives before the next launcher sweeps it, and the cap on what
// is read back out of it.
const (
	tuiConsoleStateDirName    = "tui-console"
	tuiConsoleErrorFileMaxAge = 24 * time.Hour
	tuiConsoleMaxErrorBytes   = 4096
)

// tuiConsoleStdioIsTerminal reports whether this process has a terminal to give
// tmux. Indirected for tests, which have neither.
var tuiConsoleStdioIsTerminal = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// tuiConsoleTmuxInstalled reports whether tmux is on PATH. Indirected for the
// same reason as the probes around it: a test that is not about tmux's presence
// must not silently change meaning on a machine that lacks it — CI's macOS
// runner does, and every test past this point would otherwise take the "no
// tmux" exit instead of the branch it came to check.
var tuiConsoleTmuxInstalled = func() bool {
	return session.CheckTmuxInstalled() == nil
}

// tuiConsoleHasHarnessAncestor reports whether this process is running under a
// coding harness, by the same process-tree walk the identity middleware
// classifies callers with. Indirected for tests, whose own ancestry depends on
// what ran `go test`.
var tuiConsoleHasHarnessAncestor = func() bool {
	_, hasAncestor := convIDForPID(os.Getpid())
	return hasAncestor
}

// tuiConsoleRelaunchRequested reports whether this process should become the
// launcher described at the top of this file.
//
// Both negative cases are the same rule from different directions: a console
// that is already inside tmux has the session it needs. For an operator running
// `serve --tui` from their own tmux that means the console appears in the tmux
// they are already in, rather than a second one they would have to find; for
// the inner daemon it is the recursion guard. The env check is belt and braces
// for the second case — a pane whose TMUX was stripped would otherwise fork a
// console per generation.
func tuiConsoleRelaunchRequested(p *serveParams) bool {
	if !p.TUI {
		return false
	}
	return !insideTmux() && os.Getenv(tuiConsoleSessionEnv) == ""
}

// tuiConsoleInOwnTmuxSession reports whether this daemon is the console a
// launcher started in a tmux session of its own — as opposed to one drawing
// straight onto the operator's terminal.
//
// What it decides is what may go ON that screen. A tmux pane is not the private
// terminal the operator is sitting at: its contents, scrollback included, are
// readable with `tmux -L tclaude capture-pane` by anything that can reach the
// server, which on an unsandboxed host includes the agents this very console
// spawns. So a console in a pane draws no credential — not the operator token,
// not a dashboard sign-in link. canMintDashboardLink already reasoned exactly
// this way about a console started from inside an agent's pane; the difference
// now is that an OPERATOR console can be in a pane too, and being the operator
// is no longer enough to make the screen private.
func tuiConsoleInOwnTmuxSession() bool {
	return os.Getenv(tuiConsoleSessionEnv) != ""
}

// tuiConsoleUnavailable names the reason this host cannot give the console a
// tmux session of its own, or "" when it can. Every one of them degrades to the
// pre-existing behaviour — the console in this terminal — rather than failing
// startup: the session is how the console is presented, not what it does, and a
// daemon that refused to start over it would be a worse trade than a console
// that draws where it always used to.
func tuiConsoleUnavailable(p *serveParams) string {
	// The security-relevant one, and the reason it comes first. The daemon
	// classifies its own console by process ancestry — a harness ancestor beats
	// an operator token, so `serve --tui` from an agent's pane gets an
	// agent-class console (see the identity note in tui.go). Relaunching
	// reparents the daemon under the tmux server, which ERASES that ancestor.
	// insideTmux() covers the ordinary case, but TMUX is an environment variable
	// the caller owns: `env -u TMUX tclaude agentd serve --tui` from an agent's
	// pane would otherwise come back as an operator console. Checking the
	// process tree makes the guard structural instead.
	if tuiConsoleHasHarnessAncestor() {
		return "this process runs under a coding harness, whose ancestry the console must keep"
	}
	if !tuiConsoleTmuxInstalled() {
		return "tmux is not installed"
	}
	if !tuiConsoleStdioIsTerminal() {
		// tmux cannot attach a session to something that is not a terminal, and
		// a bubbletea console on a pipe is not much of a console either — but
		// that is the caller's existing bargain (`-p`, a scraped launch), not
		// something to break here.
		return "stdin/stdout is not a terminal"
	}
	// An external tmux runtime puts the server in a separate, longer-lived
	// systemd unit precisely so it survives agentd (see docs/sandboxing.md).
	// Putting the console's session on it would run the daemon inside the
	// delegated cgroup and tie its life to a unit meant to outlive it. Resolved
	// from flag / environment / config exactly as runServe resolves it later.
	cfg, _ := config.Load()
	dir, _ := resolveResourceDelegationDir(p.ResourceDelegationDir, cfg)
	if dir = strings.TrimSpace(dir); dir != "" {
		return "an external tmux runtime owns the tclaude server (" + dir + ")"
	}
	return ""
}

// runTUIConsoleInTmux is the launcher. handled=false means it declined and the
// caller should run the console in this terminal after all; handled=true means
// the console's whole life happened inside this call and err is its outcome.
func runTUIConsoleInTmux(p *serveParams) (handled bool, err error) {
	if reason := tuiConsoleUnavailable(p); reason != "" {
		slog.Info("tui: console keeps this terminal instead of a tmux session of its own",
			"reason", reason, "module", "agentd")
		fmt.Fprintf(os.Stderr, "tclaude: %s; running the terminal UI in this window\n", reason)
		return false, nil
	}
	exe, err := os.Executable()
	if err != nil {
		slog.Warn("tui: could not resolve this executable; console keeps this terminal",
			"error", err, "module", "agentd")
		fmt.Fprintf(os.Stderr, "tclaude: could not resolve this executable (%v); running the terminal UI in this window\n", err)
		return false, nil
	}

	// The error file is how a console reports a startup failure it has no screen
	// left to print on, so a console that cannot have one is a console whose
	// failures would be invisible — which is reason enough to keep it on this
	// terminal instead, where they are not. Failing startup over it would also
	// be the wrong message: every way this can fail means the private data
	// directory is unusable, and the singleton lock and the database open are
	// moments away with errors that say so.
	errorFile, removeErrorFile, err := newTUIConsoleErrorFile()
	if err != nil {
		slog.Warn("tui: could not prepare the console's startup-error file; console keeps this terminal",
			"error", err, "module", "agentd")
		fmt.Fprintf(os.Stderr, "tclaude: could not prepare the terminal UI's error file (%v); running the terminal UI in this window\n", err)
		return false, nil
	}
	// Registered only now, so nothing is cleaned up that was never created.
	defer removeErrorFile()

	// From here the console's own tmux session is the plan, and a tmux that
	// cannot deliver it is a real failure rather than a host to route around.
	name := session.UniqueTmuxSessionName(tuiConsoleSessionBase)
	if err := startTUIConsoleSession(name, exe, errorFile); err != nil {
		return true, err
	}

	// One teardown, reachable from two directions: the attach returning below,
	// and a signal that kills the launcher outright. Without the second, an ssh
	// drop or a closed terminal window would leave the console session — and
	// the daemon inside it — running with no client and nothing left to stop
	// them, which is exactly the "nobody can see it" state the foreground
	// contract exists to prevent. The next `serve --tui` would then fail on the
	// singleton lock with no hint of where the daemon actually is.
	stop := sync.OnceFunc(func() { stopTUIConsoleSession(name) })
	stopTrapping := trapTUIConsoleSignals(stop)
	defer stopTrapping()

	attachErr := attachToTUIConsoleSession(name)
	// Unconditional: on the ordinary path the daemon has already exited and
	// this finds nothing, and on every other path — a detach, an attach that
	// never got off the ground — it is what keeps `serve --tui` the foreground
	// process it says it is.
	stop()

	// The daemon's own error outranks the client's: a console that failed to
	// start takes its session with it, which the attach then reports as
	// whatever tmux makes of a session that is not there.
	if msg := readTUIConsoleError(errorFile); msg != "" {
		return true, errors.New(msg)
	}
	if attachErr != nil {
		return true, fmt.Errorf("attach the terminal UI's tmux session %s: %w", name, attachErr)
	}
	return true, nil
}

// startTUIConsoleSession creates the detached session the console runs in.
//
// The daemon is launched through a private launch script rather than a bare
// argv because tmux gives a new pane the SERVER's environment, not the client's
// — on a server that was already up, that environment can be months old and
// from an entirely different shell. The script re-exports this process's own
// environment first, so the console starts with what the operator actually has
// (and, unlike `new-session -e`, without putting any of it in `ps` output or
// depending on a tmux new enough to take the flag).
func startTUIConsoleSession(name, exe, errorFile string) error {
	env := map[string]string{
		tuiConsoleSessionEnv:   name,
		tuiConsoleErrorFileEnv: errorFile,
	}
	// `exec` so the pane's #{pane_pid} IS the daemon: the shutdown path below
	// signals that pid, and a surviving `sh` wrapper would swallow the SIGTERM
	// the console is supposed to receive.
	launch := clcommon.BuildEnvExports(env) + "exec " + tuiConsoleArgv(exe)
	scriptPath, removeScript, err := session.WriteLaunchScript(launch)
	if err != nil {
		return fmt.Errorf("write the terminal UI's launch script: %w", err)
	}
	args := []string{"new-session", "-d", "-s", name, "-n", "console"}
	if cwd, cwdErr := os.Getwd(); cwdErr == nil && cwd != "" {
		args = append(args, "-c", cwd)
	}
	// Multi-word command → tmux execvp's it directly, with no shell-quoting
	// layer of its own in between (same shape as session launches).
	args = append(args, "sh", scriptPath)
	if out, err := clcommon.Default.Command(args...).CombinedOutput(); err != nil {
		// Only on failure: a launched script deletes itself as its first
		// statement, and removing it from here would race the `sh` that tmux
		// has just forked but may not yet have opened it.
		removeScript()
		return fmt.Errorf("create the terminal UI's tmux session %s: %w: %s",
			name, err, strings.TrimSpace(string(out)))
	}
	slog.Info("tui: console running in its own tmux session",
		"tmux_session", name, "module", "agentd")
	return nil
}

// tuiConsoleArgv reproduces this invocation for the pane: the resolved
// executable (os.Args[0] can be a bare name that only PATH resolves, and the
// pane's PATH is not necessarily this one's) with the same arguments. --tui is
// among them, which is the point — the inner run is a `serve --tui` in every
// respect except that it finds itself inside tmux.
func tuiConsoleArgv(exe string) string {
	parts := make([]string, 0, len(os.Args))
	parts = append(parts, clcommon.ShellQuoteArg(exe))
	for _, arg := range os.Args[1:] {
		parts = append(parts, clcommon.ShellQuoteArg(arg))
	}
	return strings.Join(parts, " ")
}

// attachToTUIConsoleSession gives this terminal to the console and blocks until
// the operator gets it back — because the console quit (its session is
// destroyed and tmux detaches the client) or because they detached by hand.
// Which of the two it was is stopTUIConsoleSession's question.
//
// Exit status 1 alone is not a failure, the same reading
// session.attachToSessionWithFlags applies: tmux exits 1 on a plain detach AND
// on the session ending underneath the client, which is precisely how a console
// quits. What tells those apart from a real failure is stderr — tmux writes
// "[detached (from session …)]" to STDOUT on a clean detach and keeps stderr
// empty, while "no sessions", "can't find session" and "open terminal failed"
// all land on stderr. Without that discrimination a console whose pane died
// before the daemon could write its error file would exit 0 having run nothing.
//
// stderr is teed rather than captured: the operator should still see tmux's own
// words on their terminal, and the copy is only there to be turned into an
// error.
func attachToTUIConsoleSession(name string) error {
	cmd := clcommon.Default.Command("attach-session", "-t", clcommon.ExactTarget(name))
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout = os.Stdin, os.Stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		if msg := boundTUIConsoleText(stderr.String()); msg != "" {
			return errors.New(msg)
		}
		return nil
	}
	return err
}

// trapTUIConsoleSignals runs stop on the signals that would otherwise kill the
// launcher without it, and returns the call that stops trapping.
//
// It does NOT exit: stopping the console destroys its tmux session, which
// detaches the client, which returns the attach below into the ordinary exit
// path with every deferred cleanup intact. SIGINT is included for completeness
// rather than for ctrl-c — tmux holds the terminal in raw mode while attached,
// so ctrl-c reaches the console as a keystroke, not a signal.
func trapTUIConsoleSignals(stop func()) func() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-sigs:
			slog.Info("tui: launcher signalled; stopping the console it started",
				"signal", sig.String(), "module", "agentd")
			stop()
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sigs)
		close(done)
	}
}

// stopTUIConsoleSession ends the console session if it is still there, which
// means the operator detached instead of quitting. `serve --tui` is a
// foreground process whose face is the console, so losing sight of it ends the
// run rather than leaving a daemon nobody can see.
//
// SIGTERM first, aimed at the pane's own pid, so the daemon takes its ordinary
// shutdown path — draining HTTP, flushing checkpoints, releasing the singleton
// lock. kill-session is the backstop for one that does not go.
//
// The pid is read from tmux and then signalled, so in principle the daemon
// could exit and its pid be reused in between, sending SIGTERM to a stranger.
// tuiConsolePanePID is therefore the LAST thing read before the signal — the
// window is a single syscall wide, and closing it entirely would need a pidfd
// this code has no other reason to carry.
func stopTUIConsoleSession(name string) {
	if !tuiConsoleSessionAlive(name) {
		return
	}
	slog.Info("tui: console session outlived its client; stopping the daemon inside it",
		"tmux_session", name, "module", "agentd")
	if pid := tuiConsolePanePID(name); pid > 0 {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			slog.Warn("tui: could not signal the console daemon",
				"tmux_session", name, "pid", pid, "error", err, "module", "agentd")
		} else if waitForTUIConsoleSessionGone(name) {
			slog.Info("tui: console daemon shut down",
				"tmux_session", name, "module", "agentd")
			return
		}
	}
	if out, err := clcommon.Default.Command(
		"kill-session", "-t", clcommon.ExactTarget(name)).CombinedOutput(); err != nil {
		slog.Warn("tui: could not stop the console's tmux session",
			"tmux_session", name, "error", err,
			"output", strings.TrimSpace(string(out)), "module", "agentd")
		return
	}
	slog.Info("tui: stopped the console's tmux session",
		"tmux_session", name, "module", "agentd")
}

// tuiConsoleSessionAlive reports whether the console's session still exists.
// `-N` keeps the question from being self-answering: without it a client sent
// to a dead server starts one.
func tuiConsoleSessionAlive(name string) bool {
	return clcommon.Default.Command(
		"-N", "has-session", "-t", clcommon.ExactTarget(name)).Run() == nil
}

// tuiConsolePanePID reads the pid of the process in the console's pane — the
// daemon itself, thanks to the `exec` in its launch script. 0 when tmux cannot
// answer or answers with something that is not a pid; the caller then falls
// back to kill-session.
func tuiConsolePanePID(name string) int {
	// display-message is a target-pane command, so the session must be
	// qualified with ':' for tmux to strip ExactTarget's '=' (see ExactTarget).
	out, err := clcommon.Default.Command(
		"-N", "display-message", "-p", "-t", clcommon.ExactTarget(name)+":", "#{pane_pid}").Output()
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// waitForTUIConsoleSessionGone polls for the signalled console to disappear,
// reporting whether it did within the grace window.
func waitForTUIConsoleSessionGone(name string) bool {
	deadline := time.Now().Add(tuiConsoleStopGrace)
	for {
		if !tuiConsoleSessionAlive(name) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(tuiConsoleStopPoll)
	}
}

// newTUIConsoleErrorFile creates the private file the console writes a startup
// failure to. It carries whatever runServe put in an error, so it lives under
// the 0700 private data directory — in a subdirectory of its own, like the
// launch scripts, so a launcher that never got to clean up (a SIGKILL, a lost
// host) litters a corner rather than the data root. Stale ones are swept here
// for the same reason: nothing else ever will.
func newTUIConsoleErrorFile() (string, func(), error) {
	base := strings.TrimSpace(config.DataDir())
	if base == "" {
		return "", func() {}, fmt.Errorf("resolve the private data directory for the terminal UI's error file")
	}
	dir := filepath.Join(base, tuiConsoleStateDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", func() {}, fmt.Errorf("create the terminal UI's private directory: %w", err)
	}
	sweepStaleTUIConsoleErrorFiles(dir, time.Now())
	f, err := os.CreateTemp(dir, "error-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create the terminal UI's error file: %w", err)
	}
	path := f.Name()
	remove := func() { _ = os.Remove(path) }
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		remove()
		return "", func() {}, fmt.Errorf("protect the terminal UI's error file: %w", err)
	}
	if err := f.Close(); err != nil {
		remove()
		return "", func() {}, fmt.Errorf("close the terminal UI's error file: %w", err)
	}
	return path, remove, nil
}

// sweepStaleTUIConsoleErrorFiles removes error files old enough that no live
// launcher can still be waiting on them. Best-effort throughout: a directory
// that cannot be read or an entry that cannot be removed is not a reason to
// refuse to start a console.
func sweepStaleTUIConsoleErrorFiles(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "error-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < tuiConsoleErrorFileMaxAge {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
}

// readTUIConsoleError returns what the console recorded on its way out.
func readTUIConsoleError(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return boundTUIConsoleText(string(raw))
}

// boundTUIConsoleText trims and caps text on its way into an error the launcher
// prints on the operator's terminal. Both sources are small in practice — one
// runServe error, or a line or two from tmux — and neither is worth trusting
// with the size of what it writes.
func boundTUIConsoleText(raw string) string {
	msg := strings.TrimSpace(raw)
	if len(msg) > tuiConsoleMaxErrorBytes {
		msg = msg[:tuiConsoleMaxErrorBytes]
	}
	return msg
}

// recordTUIConsoleStartupError hands a console's failure back to the launcher
// that started it. Deferred at the top of runServe, so it sees whatever runServe
// finally returns; a no-op for every run that is not a launched console.
func recordTUIConsoleStartupError(err error) {
	if err == nil {
		return
	}
	path := strings.TrimSpace(os.Getenv(tuiConsoleErrorFileEnv))
	if path == "" {
		return
	}
	if writeErr := os.WriteFile(path, []byte(err.Error()), 0o600); writeErr != nil {
		slog.Warn("tui: could not hand the console's startup failure back to its launcher",
			"error", writeErr, "module", "agentd")
	}
}
