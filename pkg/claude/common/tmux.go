package common

import (
	"log/slog"
	"os/exec"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// tmuxSocketName caches the resolved `tmux -L` socket name. It is empty until
// the first TmuxSocketName call — ResolvedTmuxSocketName never returns "", so
// the zero value is an unambiguous "not resolved yet".
var (
	tmuxSocketMu   sync.Mutex
	tmuxSocketName string
)

// TmuxSocketName returns the named socket for tclaude's independent tmux
// server — config tmux.socket_name, defaulting to "tclaude".
//
// The config file is read at most once per process and the answer is cached:
// every tmux command in a process must target the same server, and re-reading
// mid-run would let a config edit split a running daemon's commands across two
// sockets. Changing the name therefore takes effect for newly started
// processes only.
func TmuxSocketName() string {
	tmuxSocketMu.Lock()
	defer tmuxSocketMu.Unlock()
	if tmuxSocketName == "" {
		cfg, err := config.Load()
		if err != nil {
			// Load already logged the parse/read failure, but its message says
			// nothing about tmux. Falling back to the default socket when a
			// name WAS configured moves this process onto a different tmux
			// server, where it finds none of the running panes — say so, or the
			// operator has no way to connect "I broke a comma in config.json"
			// to "all my agents vanished".
			slog.Warn("Unable to read tmux.socket_name; falling back to the default tmux server",
				"socket", config.DefaultTmuxSocketName,
				"hint", "if config.json sets a socket name, fix the file and restart tclaude",
				"err", err)
		}
		tmuxSocketName = cfg.ResolvedTmuxSocketName()
	}
	return tmuxSocketName
}

// SetTmuxSocketNameForTest pins the resolved socket name and returns a restore
// func that puts the previous value back. Tests only — it exists so a test can
// exercise a non-default socket without writing an operator config file. The
// empty string clears the cache instead of pinning, so a test that has pointed
// HOME at a fixture directory can force the next call to re-read the config.
func SetTmuxSocketNameForTest(name string) func() {
	tmuxSocketMu.Lock()
	defer tmuxSocketMu.Unlock()
	previous := tmuxSocketName
	tmuxSocketName = name
	return func() {
		tmuxSocketMu.Lock()
		defer tmuxSocketMu.Unlock()
		tmuxSocketName = previous
	}
}

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

// LiveTmux is the production impl: forks `tmux -L <socket> <args>`.
// Exported so tests can wrap it (e.g., a recording proxy that
// forwards to LiveTmux for some calls and to a fake for others).
type LiveTmux struct{}

// Command builds an exec.Cmd that invokes the real tmux binary.
func (LiveTmux) Command(args ...string) *exec.Cmd {
	return exec.Command("tmux", TmuxArgs(args...)...)
}

// ListSessions forks one tmux list-sessions command and returns session names
// whose active pane is not retained-dead. Non-zero exit (typically
// "no server running on …" when the tmux server is down) collapses to
// an empty set with nil error — the snapshot semantics are the same as
// "every session is offline".
func (l LiveTmux) ListSessions() (map[string]struct{}, error) {
	out, err := l.Command("list-sessions", "-F", "#{session_name}\t#{pane_dead}").Output()
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
		fields := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		name := fields[0]
		if name == "" || (len(fields) == 2 && fields[1] == "1") {
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

// TmuxArgs prepends `-L <socket>` to the given tmux arguments.
func TmuxArgs(args ...string) []string {
	return append([]string{"-L", TmuxSocketName()}, args...)
}

// TmuxHyperlinksFeature is the tmux terminal-feature name for OSC 8 hyperlink
// passthrough. tmux always PARSES OSC 8 out of pane output and keeps the target
// URL in its grid, but it only re-emits the sequence to a client whose terminal
// advertises the `Hls` capability. Neither the terminfo entry for the web
// terminal's TERM nor tmux's own terminal auto-detection (xterm.js answers no
// XTVERSION query) supplies it, so without an explicit opt-in every OSC 8 link
// a harness draws reaches the browser as plain, unclickable label text.
const TmuxHyperlinksFeature = "hyperlinks"

// TmuxClientFeaturesEnv asks a `tclaude session attach` child process to pass
// tmux `-T <features>` for the client it forks. The dashboard's web terminals
// reach tmux through that wrapper rather than by spawning tmux themselves, so
// the "this client renders OSC 8" fact has to survive one process hop. It is
// deliberately an env var and not a CLI flag: the native-terminal attach path
// shares the same command builder and must keep tmux's default (detected)
// feature set, since an arbitrary local terminal may not handle OSC 8.
const TmuxClientFeaturesEnv = "TCLAUDE_TMUX_CLIENT_FEATURES"

// TmuxClientFeatureArgs turns a requested feature list into the tmux client
// flags that precede the command word (`tmux -T a,b attach-session …`), or nil
// when nothing valid was asked for.
//
// The list is charset-gated because it arrives from the process environment and
// is forwarded to a subprocess argv. It is not load-bearing for tmux itself —
// tmux ignores a feature name it does not recognise and starts normally — but a
// value that reaches this function unrecognisable is a sign the caller is not
// the one we think it is, and passing it on would only widen what an
// environment writer can put in front of tmux.
func TmuxClientFeatureArgs(features string) []string {
	features = strings.TrimSpace(features)
	if features == "" || len(features) > 128 {
		return nil
	}
	for _, r := range features {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == ',' || r == '-' {
			continue
		}
		return nil
	}
	return []string{"-T", features}
}

// ExactTarget returns a tmux -t target that resolves the session name
// EXACTLY. A bare `-t name` falls back to prefix (then fnmatch) matching
// when no exact match exists, so a command aimed at a dead session's name
// can silently land on a live session sharing that prefix — "myrepo"
// (dead) resolving to "myrepo-2" (alive) would misroute an attach, a
// kill, or injected keystrokes. The leading '=' pins resolution to
// exact-only.
//
// CRUCIAL: tmux parses the '=' marker off the SESSION (and window) parts
// of a target only (cmd-find.c "Set exact match flags"). A bare
// ExactTarget(name) is therefore valid ONLY for commands whose -t is a
// target-session (has-session, kill-session, attach-session,
// switch-client, list-clients, detach-client -s). For a target-pane /
// target-window command (send-keys, display-message, capture-pane,
// set-option, list-panes, paste-buffer) a colon-less target lands whole
// in the pane/window slot where the '=' is NEVER stripped — the lookup
// then hunts for a pane literally named "=name" and fails (or, with
// CANFAIL commands, silently acts on the "current" state). For those,
// qualify the target: ExactTarget(name) + ":" pins the session exactly
// and keeps tmux's "current window / active pane" resolution (an empty
// window/pane part is the same as none — cmd-find.c "Empty is the same
// as NULL"), and ExactTarget(name) + ":0.0" addresses window 0 pane 0
// explicitly.
func ExactTarget(sessionName string) string {
	return "=" + sessionName
}
