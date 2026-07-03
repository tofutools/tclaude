package common

import (
	"os/exec"
	"strings"
)

// TmuxSocketName is the named socket for tclaude's independent tmux server.
const TmuxSocketName = "tclaude"

// Tmux is the boundary surface flow tests inject through. The default
// LiveTmux runs the real tmux binary; tests assign a fake to Default
// at setup and restore via t.Cleanup.
type Tmux interface {
	Command(args ...string) *exec.Cmd
	// ListSessions returns the set of session names currently alive on
	// the tclaude tmux server, in ONE call. Snapshot-shaped callers
	// (dashboard poll, group/peer list handlers) fetch this once and
	// then test individual session liveness via map lookup, avoiding
	// per-row `has-session` subprocess fan-out.
	//
	// A nil/empty map with err==nil means "no server, no sessions" —
	// callers should treat both as "everything is offline". A non-nil
	// err means the listing itself failed (parse, exec) — distinct
	// from "no server running" which is a normal state.
	ListSessions() (map[string]struct{}, error)
}

// Default is the package-wide Tmux instance every caller hits via the
// TmuxCommand facade. Production starts on LiveTmux; tests overwrite
// during their setup. Single global var = goroutine-unsafe across
// parallel tests on the same package — flow tests don't t.Parallel.
var Default Tmux = LiveTmux{}

// LiveTmux is the production impl: forks `tmux -L tclaude <args>`.
// Exported so tests can wrap it (e.g., a recording proxy that
// forwards to LiveTmux for some calls and to a fake for others).
type LiveTmux struct{}

// Command builds an exec.Cmd that invokes the real tmux binary.
func (LiveTmux) Command(args ...string) *exec.Cmd {
	return exec.Command("tmux", TmuxArgs(args...)...)
}

// ListSessions forks one `tmux -L tclaude list-sessions -F '#{session_name}'`
// and returns the set of alive session names. Non-zero exit (typically
// "no server running on …" when the tmux server is down) collapses to
// an empty set with nil error — the snapshot semantics are the same as
// "every session is offline".
func (l LiveTmux) ListSessions() (map[string]struct{}, error) {
	out, err := l.Command("list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		// `tmux ls` exits non-zero when there is no server. Treat that
		// as the empty set rather than an error — it is the normal
		// "nothing is running" state, not a probe failure.
		if _, ok := err.(*exec.ExitError); ok {
			return map[string]struct{}{}, nil
		}
		return nil, err
	}
	alive := map[string]struct{}{}
	for line := range strings.SplitSeq(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		alive[name] = struct{}{}
	}
	return alive, nil
}

// TmuxCommand is a thin facade over Default.Command. Kept so the
// 48 existing call sites don't need to be rewritten in this diff;
// new code is welcome to call clcommon.Default.Command directly.
func TmuxCommand(args ...string) *exec.Cmd {
	return Default.Command(args...)
}

// TmuxArgs prepends -L tclaude to the given tmux arguments.
func TmuxArgs(args ...string) []string {
	return append([]string{"-L", TmuxSocketName}, args...)
}

// ExactTarget returns a tmux -t target that resolves the session name
// EXACTLY. A bare `-t name` falls back to prefix (then fnmatch) matching
// when no exact match exists, so a command aimed at a dead session's name
// can silently land on a live session sharing that prefix — "myrepo"
// (dead) resolving to "myrepo-2" (alive) would misroute an attach, a
// kill, or injected keystrokes. The leading '=' pins resolution to
// exact-only. Use it for every -t that targets a session by name; a
// window/pane suffix may be appended (ExactTarget(name) + ":0.0") — the
// '=' binds to the session part.
func ExactTarget(sessionName string) string {
	return "=" + sessionName
}
