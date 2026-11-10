package common

import (
	"errors"
	"os/exec"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/tmuxctl"
)

// TmuxOutput runs a tmux command and returns its stdout, like
// TmuxCommand(args...).Output(). TmuxRun and TmuxCombinedOutput are the Run
// and CombinedOutput twins.
//
// When agentd's control-mode connection is up and the command is one that
// means the same over it (see controlRoute), it is sent there instead of
// forking tmux. Everything else, and every call while the connection is
// down, takes the exec path through Default.Command, so the flow-test seam is
// unchanged.
//
// The error differs by path. Exec returns *exec.ExitError (tmux's stderr is
// in ExitError.Stderr for TmuxOutput, and in the output for
// TmuxCombinedOutput); control mode returns *TmuxCommandError, whose message
// carries tmux's text. A caller that needs to tell the two apart, or reads
// ExitError, must keep using TmuxCommand.
func TmuxOutput(args ...string) ([]byte, error) {
	if out, err, ok := routeControl(args); ok {
		return out, err
	}
	return Default.Command(args...).Output()
}

// TmuxRun runs a tmux command for its effect, like TmuxCommand(args...).Run().
func TmuxRun(args ...string) error {
	if _, err, ok := routeControl(args); ok {
		return err
	}
	return Default.Command(args...).Run()
}

// TmuxCombinedOutput runs a tmux command and returns stdout and stderr, like
// TmuxCommand(args...).CombinedOutput(). Over control mode a rejected
// command's message is the output.
func TmuxCombinedOutput(args ...string) ([]byte, error) {
	if out, err, ok := routeControl(args); ok {
		var cmdErr *TmuxCommandError
		if errors.As(err, &cmdErr) {
			return []byte(cmdErr.Message() + "\n"), err
		}
		return out, err
	}
	return Default.Command(args...).CombinedOutput()
}

// TmuxCommandError is a command tmux rejected over control mode (its %error
// reply). Error() reads like the exec path's "exit status 1: <stderr>" detail
// so callers matching tmux's text ("can't find session", "no such window")
// keep working.
type TmuxCommandError struct {
	lines []string
}

// IsTmuxCommandFailure reports whether err says tmux ran and rejected the
// command, on either path: an *exec.ExitError from a tmux process, or a
// *TmuxCommandError from control mode.
func IsTmuxCommandFailure(err error) bool {
	var exitErr *exec.ExitError
	var cmdErr *TmuxCommandError
	return errors.As(err, &exitErr) || errors.As(err, &cmdErr)
}

// Message is tmux's own error text.
func (e *TmuxCommandError) Message() string { return strings.Join(e.lines, "\n") }

func (e *TmuxCommandError) Error() string { return "tmux: " + strings.Join(e.lines, "; ") }

// TryTmuxControl sends args over control mode when the connection is up and
// the command routes there; ok=false means the caller should run it over
// exec as it otherwise would. It is for callers that wrap exec themselves
// (a deadline, their own stderr handling).
func TryTmuxControl(args ...string) (out []byte, err error, ok bool) {
	return routeControl(args)
}

// routeControl sends args over control mode when that is possible and
// equivalent. ok=false means "use exec".
func routeControl(args []string) ([]byte, error, bool) {
	ct, isControl := Default.(*ControlTmux)
	if !isControl || !ct.client.Connected() {
		return nil, nil, false
	}
	return ct.run(args)
}

func (t *ControlTmux) run(args []string) ([]byte, error, bool) {
	// -N only stops a client from starting a server; with a control
	// connection up the server exists.
	if len(args) > 0 && args[0] == "-N" {
		args = args[1:]
	}
	kind := controlRoute(args)
	if kind == routeExec {
		return nil, nil, false
	}
	lines, err := t.client.Run(args...)
	if err == nil {
		t.countRouted(args[0])
		return joinReply(lines), nil, true
	}
	var cmdErr *tmuxctl.CommandError
	switch {
	case errors.As(err, &cmdErr):
		t.countRouted(args[0])
		return nil, &TmuxCommandError{lines: cmdErr.Lines}, true
	case errors.Is(err, tmuxctl.ErrNotConnected), errors.Is(err, tmuxctl.ErrInvalidCommand):
		// Never written: exec is a clean retry.
		return nil, nil, false
	case kind == routeQuery:
		// Sent, outcome unknown. A query is safe to ask again.
		return nil, nil, false
	default:
		// A mutation that may have happened must not run twice (a send-keys
		// typed twice, say); report the failure instead.
		return nil, err, true
	}
}

func joinReply(lines []string) []byte {
	if len(lines) == 0 {
		return nil
	}
	n := 0
	for _, l := range lines {
		n += len(l) + 1
	}
	b := make([]byte, 0, n)
	for _, l := range lines {
		b = append(b, l...)
		b = append(b, '\n')
	}
	return b
}

type routeKind int

const (
	routeExec routeKind = iota
	routeQuery
	routeMutation
)

// controlTargetRule says what makes a command safe to send over control mode.
// The control client is itself a client attached to the hidden session, so a
// command that falls back to "the current client / session / pane" when it
// has no target would act on that hidden session instead of on what an exec
// client (which has no current session) resolves to. Only commands with an
// explicit target, a server-global (-g) scope, or an all-sessions (-a) scope
// are routed.
type controlTargetRule struct {
	kind routeKind
	// target: an explicit -t is enough.
	target bool
	// global: a -g flag is enough.
	global bool
	// all: a -a flag is enough.
	all bool
	// client: an explicit -c is required (it names the client to act on).
	client bool
	// session: -s names the session (detach-client -s).
	session bool
	// print: -p is required (display-message, capture-pane print to the
	// reply only with -p).
	print bool
	// none: no target needed (the command is server-wide either way).
	none bool
}

var controlRoutes = map[string]controlTargetRule{
	// queries
	"has-session":      {kind: routeQuery, target: true},
	"display-message":  {kind: routeQuery, target: true, print: true},
	"capture-pane":     {kind: routeQuery, target: true, print: true},
	"list-panes":       {kind: routeQuery, target: true, all: true},
	"list-windows":     {kind: routeQuery, target: true, all: true},
	"list-clients":     {kind: routeQuery, target: true},
	"list-sessions":    {kind: routeQuery, none: true},
	"show-options":     {kind: routeQuery, target: true, global: true},
	"show-hooks":       {kind: routeQuery, target: true, global: true},
	"show-environment": {kind: routeQuery, target: true, global: true},
	// mutations
	"kill-session":    {kind: routeMutation, target: true},
	"kill-pane":       {kind: routeMutation, target: true},
	"set-option":      {kind: routeMutation, target: true, global: true},
	"set-hook":        {kind: routeMutation, target: true, global: true},
	"set-environment": {kind: routeMutation, target: true, global: true},
	"send-keys":       {kind: routeMutation, target: true},
	"resize-window":   {kind: routeMutation, target: true},
	"copy-mode":       {kind: routeMutation, target: true},
	"detach-client":   {kind: routeMutation, target: true, session: true},
	"switch-client":   {kind: routeMutation, client: true},
}

// controlRoute classifies args for control mode. Anything not positively
// known to mean the same thing there stays on exec: a leading client flag, a
// ";" command separator (exec passes it as tmux's separator, control mode
// would quote it), a format naming client_* variables (they would describe
// the control client), and every command not in controlRoutes (new-session
// takes the calling process's environment and cwd, kill-server, run-shell,
// wait-for, load-buffer, …).
func controlRoute(args []string) routeKind {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return routeExec
	}
	rule, ok := controlRoutes[args[0]]
	if !ok {
		return routeExec
	}
	var hasT, hasG, hasA, hasC, hasS, hasP bool
	for _, a := range args[1:] {
		if a == ";" || a == `\;` {
			return routeExec
		}
		// list-clients' client_* fields describe the listed clients; in any
		// other command's format they would describe the control client.
		if args[0] != "list-clients" && strings.Contains(a, "client_") {
			return routeExec
		}
		if a == "-t" {
			hasT = true
			continue
		}
		if a == "-c" {
			hasC = true
			continue
		}
		if a == "-s" {
			hasS = true
			continue
		}
		if len(a) >= 2 && a[0] == '-' && a[1] != '-' && isFlagCluster(a[1:]) {
			hasG = hasG || strings.ContainsRune(a, 'g')
			hasA = hasA || strings.ContainsRune(a, 'a')
			hasP = hasP || strings.ContainsRune(a, 'p')
			hasT = hasT || strings.HasSuffix(a, "t")
		}
	}
	if rule.print && !hasP {
		return routeExec
	}
	if rule.client && !hasC {
		return routeExec
	}
	if rule.none || (rule.target && hasT) || (rule.global && hasG) || (rule.all && hasA) || (rule.session && hasS) || (rule.client && hasC) {
		return rule.kind
	}
	return routeExec
}

func isFlagCluster(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

// countRouted records a command answered over control mode, next to the
// subprocess counters it replaces.
func (t *ControlTmux) countRouted(name string) {
	t.mu.Lock()
	if t.routed == nil {
		t.routed = map[string]int64{}
	}
	t.routed[name]++
	t.mu.Unlock()
}
