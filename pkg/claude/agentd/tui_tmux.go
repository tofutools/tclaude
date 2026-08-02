package agentd

// tmux-server lifetime for `tclaude agentd serve --tui`. The console is the
// daemon's whole face and quitting it stops the daemon, so under --tui the
// daemon brings the `-L tclaude` tmux server up empty at startup and kills it
// again on the way out, instead of leaving it to appear and disappear
// underneath the console as agents come and go.
//
// Ownership is strictly limited to a server this run actually created: a server
// that was already up when the console started belongs to whoever started it,
// and is left untouched in both directions. Everything below exists to keep
// that line sharp.
//
// This is the LOCAL console only. The remote terminal dashboard
// (`tclaude agent tui-dashboard`) is an HTTP client of somebody else's daemon —
// the tmux server it would touch is its own host's, not the one its agents live
// on — so it never runs any of this.

import (
	"errors"
	"log/slog"
	"os/exec"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

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
// one, and returns the teardown that kills it again plus whether ownership was
// taken at all. The teardown is a no-op unless this call is what created the
// server — an external runtime, a server that predates the console, a probe
// that could not tell, and a start that failed all leave nothing to tear down,
// so `serve --tui` never kills a server it did not put there.
//
// The ownership flag is not just bookkeeping: it decides whether quitting the
// console is about to take live sessions with it, which is what the quit
// confirmation has to tell the operator (see tuiStartup.ownsTmuxServer).
//
// Failing to start one is a warning rather than a fatal: the daemon still
// serves its API, and every tmux call that follows behaves exactly as it did
// before this ownership existed (starting a server implicitly when it needs
// one). What the operator loses is the empty-but-alive server, not the daemon.
func startTUITmuxServer() (stop func(), owned bool) {
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

	return func() {
		pid, state := tuiTmuxServerPID()
		switch state {
		case tuiTmuxProbeNone:
			slog.Info("tui: tclaude tmux server is already gone; nothing to shut down",
				"server_pid", ownerPID, "module", "agentd")
			return
		case tuiTmuxProbeUnknown:
			// Same rule as at startup, for the same reason: without an answer there
			// is no way to tell our server from a stranger's, and kill-server does
			// not ask. Leaving it costs an idle server (see the exit-empty note in
			// docs/dashboard.md); killing it could cost an operator's agents.
			slog.Warn("tui: could not confirm the tclaude tmux server is still ours; leaving it running",
				"server_pid", ownerPID, "module", "agentd")
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
			return
		}
		if out, err := clcommon.Default.Command("kill-server").CombinedOutput(); err != nil {
			slog.Warn("tui: could not shut down the tclaude tmux server",
				"error", err, "output", strings.TrimSpace(string(out)), "module", "agentd")
			return
		}
		slog.Info("tui: shut down the tclaude tmux server", "server_pid", pid, "module", "agentd")
	}, true
}
