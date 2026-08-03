package agentd

// tmux-server lifetime for `tclaude agentd serve --tui`. The console is the
// daemon's whole face and quitting it stops the daemon, so under --tui the
// daemon brings the `-L tclaude` tmux server up empty at startup and, if it is
// still empty on the way out, kills it again — instead of leaving it to appear
// and disappear underneath the console as agents come and go.
//
// Ownership is strictly limited to a server this run actually created: a server
// that was already up when the console started belongs to whoever started it,
// and is left untouched in both directions. Everything below exists to keep
// that line sharp.
//
// Ownership alone is not enough to kill, either: the teardown also requires the
// server to be EMPTY. Every condition has to hold at once, so a server this
// daemon started but that still carries sessions — agents the operator spawned
// from the console and wants to keep, a shell they opened on the socket — is
// left running, and the operator is told which of the two happened.
//
// This is the LOCAL console only. The remote terminal dashboard
// (`tclaude agent tui-dashboard`) is an HTTP client of somebody else's daemon —
// the tmux server it would touch is its own host's, not the one its agents live
// on — so it never runs any of this.

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// tmuxServerOwnershipRequested reports whether this daemon should tie the
// tclaude tmux server's lifetime to its own — `--own-tmux-server`, which is off
// by default and needs the console to attach that lifetime to. The flag without
// --tui is inert (runServe says so on the way past) rather than an error: it is
// the kind of thing that ends up in a shell alias or a unit file next to flags
// that do apply, and refusing to start over it would be worse than ignoring it.
func tmuxServerOwnershipRequested(p *serveParams) bool {
	return p.TUI && p.OwnTmuxServer
}

// tuiTmuxServerArgs is the single tmux invocation that starts the server and
// pins it there. Both commands deliberately run on ONE client connection: a
// bare `start-server` in its own process leaves a server holding no sessions
// with tmux's default `exit-empty on`, which exits again the moment that client
// disconnects — before a second process could set the option. tmux only
// considers the empty-exit once no clients remain, so setting exit-empty off
// while the same client is still attached closes that window.
func tuiTmuxServerArgs() []string {
	return []string{"start-server", ";", "set-option", "-g", "exit-empty", "off"}
}

// What the probe below managed to establish. The three states exist because
// "there is no server" and "I could not find out" must not be the same answer:
// this probe is the only thing standing between the teardown and an operator's
// running agents, so every consumer treats tuiTmuxProbeUnknown as a reason to
// keep its hands off rather than as a licence to act.
type tuiTmuxProbe int

const (
	// tuiTmuxProbeUnknown: the probe failed in a way that says nothing about
	// whether a server is running — tmux missing, too old to take -N, a
	// permission error, output that is not a pid.
	tuiTmuxProbeUnknown tuiTmuxProbe = iota
	// tuiTmuxProbeNone: tmux said, in as many words, that no server is running.
	tuiTmuxProbeNone
	// tuiTmuxProbeRunning: a server answered and its pid was read.
	tuiTmuxProbeRunning
)

// tmuxNoServerStderr is how tmux reports the absence of a server ("no server
// running on /tmp/tmux-1000/tclaude"). It is the one failure this code is
// willing to interpret; anything else it does not recognise stays Unknown.
const tmuxNoServerStderr = "no server running"

// tuiTmuxLiveSessions counts the sessions on the tclaude tmux server that still
// have something running in them, with the same three-state discipline as the
// pid probe: an answer we could not read is Unknown, not zero.
//
// It asks tmux directly rather than going through clcommon.Default.ListSessions,
// which is shaped for the dashboard's snapshot and is wrong here twice over.
// First, ListSessions collapses ANY non-zero exit to the empty set — correct for
// a poller that only wants "everything is offline", fatal for a check that turns
// the empty set into kill-server, since a socket blip would read as "no sessions"
// and take the operator's agents with it. Second, it formats `#{pane_dead}` on
// `list-sessions`, where tmux resolves a pane-scoped variable against the
// session's CURRENT window's active pane: a session whose window 0 holds a
// retained-dead harness pane reads as offline even when another window of the
// same session is running something. Killing on that would be exactly the loss
// this check exists to prevent.
//
// `list-panes -a` avoids both: it enumerates every pane on the server with its
// own session name, so a session counts as live when ANY of its panes is, and
// the error handling below is this function's own. A session whose panes are all
// retained-dead is a husk holding scrollback, not work — it does not keep the
// server up, which is what makes the ordinary "agent ran, exited, operator quit"
// path still end in a shut-down server.
//
// `-N` keeps the probe from starting the very server it is asking about, and
// stdout is read on its own so a warning on stderr cannot be parsed as panes.
func tuiTmuxLiveSessions() (int, tuiTmuxProbe) {
	out, err := clcommon.Default.Command(
		"-N", "list-panes", "-a", "-F", "#{session_name}\t#{pane_dead}").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) &&
			strings.Contains(string(exitErr.Stderr), tmuxNoServerStderr) {
			return 0, tuiTmuxProbeNone
		}
		return 0, tuiTmuxProbeUnknown
	}
	live := map[string]struct{}{}
	for line := range strings.SplitSeq(string(out), "\n") {
		name, dead, ok := strings.Cut(strings.TrimSpace(line), "\t")
		// A line we cannot split is a format we do not recognise. Counting it as
		// a live session errs toward leaving the server up, which is the safe
		// direction everywhere else in this file too.
		if name == "" {
			continue
		}
		if ok && dead == "1" {
			continue
		}
		live[name] = struct{}{}
	}
	return len(live), tuiTmuxProbeRunning
}

// releaseTUITmuxExitEmpty hands a server this daemon is walking away from back
// to tmux's own lifetime rule.
//
// startTUITmuxServer pins `exit-empty off` so an empty server survives with no
// agents on it, and the kill is otherwise the only thing that undoes that. A
// server left running with `exit-empty off` still set would never exit on its
// own once its last session ended — and by this file's own ownership rule the
// next `--tui --own-tmux-server` run finds it already up, treats it as somebody
// else's, and neither adopts nor kills it. The flag would quietly stop working
// until the operator noticed a stray tmux process and killed it by hand.
//
// Restoring the option is safe in a way killing is not: it only ever lets an
// EMPTY server exit, so it cannot cost a session. Failure is logged and ignored —
// there is nothing better to do from a process that is on its way out.
func releaseTUITmuxExitEmpty(pid string) {
	if out, err := clcommon.Default.Command("set-option", "-gu", "exit-empty").CombinedOutput(); err != nil {
		slog.Warn("tui: could not restore exit-empty on the tclaude tmux server",
			"error", err, "output", strings.TrimSpace(string(out)),
			"server_pid", pid, "module", "agentd")
	}
}

// tuiTmuxServerPID reports the pid of an already-running tclaude tmux server.
// `-N` is what makes this a probe rather than a launch: without it the client
// would start the very server it is asking about.
//
// stdout is read on its own (not CombinedOutput) so a warning line on stderr
// cannot corrupt the pid, and the pid is charset-checked because it is later
// compared against the one we started: anything that is not a bare decimal pid
// is Unknown rather than trusted.
func tuiTmuxServerPID() (string, tuiTmuxProbe) {
	// Output() collects stderr into ExitError.Stderr as long as Stderr is unset,
	// which is what lets the "no server" case be recognised without merging that
	// text into the pid.
	out, err := clcommon.Default.Command("-N", "display-message", "-p", "#{pid}").Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) &&
			strings.Contains(string(exitErr.Stderr), tmuxNoServerStderr) {
			return "", tuiTmuxProbeNone
		}
		return "", tuiTmuxProbeUnknown
	}
	pid := strings.TrimSpace(string(out))
	if pid == "" {
		return "", tuiTmuxProbeUnknown
	}
	for _, r := range pid {
		if r < '0' || r > '9' {
			return "", tuiTmuxProbeUnknown
		}
	}
	return pid, tuiTmuxProbeRunning
}

// startTUITmuxServer starts the console's tmux server, if there is not already
// one, and returns the teardown that kills an empty one again plus whether
// ownership was taken at all. The teardown is a no-op unless this call is what
// created the server — an external runtime, a server that predates the console,
// a probe that could not tell, and a start that failed all leave nothing to tear
// down, so `serve --tui` never kills a server it did not put there, and even the
// one it did put there survives if anything is still running on it.
//
// The ownership flag is not just bookkeeping: it decides whether quitting the
// console is about to end the tmux server, which is what the quit confirmation
// has to tell the operator (see tuiStartup.ownsTmuxServer).
//
// notify is where the teardown narrates its own outcome. It is a parameter and
// not the startup writer because the two go to different places: startup lines
// are discarded under --tui (the console owns the terminal — see serveStdout),
// while the teardown runs after the console has given the terminal back, and
// what it decided about the operator's sessions is exactly the kind of thing
// that must not be buried in output.log.
//
// Failing to start one is a warning rather than a fatal: the daemon still
// serves its API, and every tmux call that follows behaves exactly as it did
// before this ownership existed (starting a server implicitly when it needs
// one). What the operator loses is the empty-but-alive server, not the daemon.
func startTUITmuxServer(notify io.Writer) (stop func(), owned bool) {
	noop := func() {}

	// External resource delegation puts the tmux server in a separate,
	// longer-lived systemd unit precisely so it survives agentd (see
	// docs/sandboxing.md). Starting one here would create it inside agentd's own
	// cgroup, and killing it on exit would take down panes that are supposed to
	// outlive this process — so in that mode the console owns nothing.
	if dir := session.ExternalResourceDelegationDir(); dir != "" {
		slog.Info("tui: external tmux runtime owns the tclaude server; not starting or killing it",
			"resource_delegation_dir", dir, "module", "agentd")
		return noop, false
	}

	switch pid, state := tuiTmuxServerPID(); state {
	case tuiTmuxProbeRunning:
		// A server that is already up came from somewhere else — an earlier
		// daemon, a `tclaude session new` from a plain shell, an operator's own
		// tmux. Its sessions are not this console's to end, so the console
		// neither re-starts it (a no-op anyway) nor touches its exit-empty, and
		// above all does not kill it on the way out.
		slog.Info("tui: tclaude tmux server was already running; leaving it to its owner",
			"server_pid", pid, "module", "agentd")
		return noop, false
	case tuiTmuxProbeUnknown:
		// Ownership is claimed only on a definite "nothing is running". Taking it
		// on a maybe would put both halves of this at risk at once: exit-empty
		// would be set on a server that may not be ours, and the teardown would
		// aim kill-server at whatever answers. Declining costs the operator the
		// empty-but-alive server and nothing else — tmux still starts a server
		// implicitly the first time something needs one, exactly as it did before
		// this ownership existed.
		slog.Warn("tui: could not determine whether a tclaude tmux server is running; not taking ownership",
			"module", "agentd")
		return noop, false
	}

	if out, err := clcommon.Default.Command(tuiTmuxServerArgs()...).CombinedOutput(); err != nil {
		slog.Warn("tui: could not start the tclaude tmux server",
			"error", err, "output", strings.TrimSpace(string(out)), "module", "agentd")
		return noop, false
	}
	// The pid of what we just started. It is the teardown's proof of identity:
	// the probe above and the start here are two processes, so a server could in
	// principle have appeared in between and been adopted by our start-server.
	// Recording the pid means the kill can be aimed at the server this daemon is
	// actually responsible for.
	ownerPID, ownerState := tuiTmuxServerPID()
	slog.Info("tui: started the tclaude tmux server with exit-empty off",
		"server_pid", ownerPID, "module", "agentd")

	// Every outcome below narrates itself to the operator. The console is gone
	// by now and its slog lines land in output.log, so this is the only place
	// they learn what happened to the server their agents were on — and "left
	// running" has to be as loud as "shut down", or silence reads as "this
	// daemon never owned a server at all".
	return func() {
		pid, state := tuiTmuxServerPID()
		switch state {
		case tuiTmuxProbeNone:
			slog.Info("tui: tclaude tmux server is already gone; nothing to shut down",
				"server_pid", ownerPID, "module", "agentd")
			fmt.Fprintln(notify, "tmux server was already gone; nothing to shut down")
			return
		case tuiTmuxProbeUnknown:
			// Same rule as at startup, for the same reason: without an answer there
			// is no way to tell our server from a stranger's, and kill-server does
			// not ask. Leaving it costs an idle server (see the exit-empty note in
			// docs/dashboard.md); killing it could cost an operator's agents.
			slog.Warn("tui: could not confirm the tclaude tmux server is still ours; leaving it running",
				"server_pid", ownerPID, "module", "agentd")
			fmt.Fprintln(notify,
				"tmux server left running: could not confirm it is the one this daemon started")
			return
		}
		// A pid we could not read at startup is not a reason to hold back here:
		// the probe before the start said, definitively, that no server was
		// running, so this run is what put one there. Only a pid that was read
		// AND no longer matches means the server we started died and something
		// else took the socket.
		if ownerState == tuiTmuxProbeRunning && pid != ownerPID {
			slog.Info("tui: tclaude tmux server was replaced by another; leaving it running",
				"started_pid", ownerPID, "running_pid", pid, "module", "agentd")
			fmt.Fprintln(notify, "tmux server left running: it is not the one this daemon started")
			return
		}
		// From here the server is ours to end. That settles WHETHER we may kill
		// it; the last condition settles whether we should, and both have to hold.
		//
		// Ownership says nothing about what the operator put on the server in the
		// meantime, and by the time this runs the console is gone — an agent still
		// working in a pane would be killed with no chance to object. Leaving a
		// non-empty server costs an idle tmux process the operator can end
		// themselves; killing it costs whatever was running on it.
		//
		// The read and the kill are two calls, so a session created in between —
		// a background sweep relaunching a pane (serve.go says outright that
		// those are signalled but not awaited), a `tclaude session new` from
		// another shell — still dies. Narrowing that window further would mean
		// holding the server still, which nothing here can do; what it does mean
		// is that this check is a guard against the sessions an operator has, not
		// a lock against the ones they are creating as the daemon exits.
		live, sessionState := tuiTmuxLiveSessions()
		switch sessionState {
		case tuiTmuxProbeNone:
			// The server answered the pid probe and was gone moments later.
			slog.Info("tui: tclaude tmux server disappeared before it could be shut down",
				"server_pid", pid, "module", "agentd")
			fmt.Fprintln(notify, "tmux server was already gone; nothing to shut down")
			return
		case tuiTmuxProbeUnknown:
			slog.Warn("tui: could not list the tclaude tmux server's panes; leaving it running",
				"server_pid", pid, "module", "agentd")
			fmt.Fprintln(notify,
				"tmux server left running: could not check whether it still has sessions")
			releaseTUITmuxExitEmpty(pid)
			return
		}
		if live > 0 {
			slog.Info("tui: tclaude tmux server still has live sessions; leaving it running",
				"sessions", live, "server_pid", pid, "module", "agentd")
			fmt.Fprintf(notify, "tmux server left running: %s still on it (it will exit when they do)\n",
				tuiTmuxSessionCount(live))
			releaseTUITmuxExitEmpty(pid)
			return
		}
		if out, err := clcommon.Default.Command("kill-server").CombinedOutput(); err != nil {
			slog.Warn("tui: could not shut down the tclaude tmux server",
				"error", err, "output", strings.TrimSpace(string(out)), "module", "agentd")
			fmt.Fprintf(notify, "tmux server could not be shut down: %v\n", err)
			return
		}
		slog.Info("tui: shut down the tclaude tmux server", "server_pid", pid, "module", "agentd")
		fmt.Fprintln(notify, "tmux server shut down: it had no sessions left on it")
	}, true
}

// tuiTmuxSessionCount renders the session count for the operator-facing line
// above. Worth the three lines: that line is the only place they learn why the
// server outlived the console, and "1 sessions" reads as a bug in the thing
// that just declined to kill their agents.
func tuiTmuxSessionCount(n int) string {
	if n == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", n)
}
