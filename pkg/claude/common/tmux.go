package common

import (
	"fmt"
	"os/exec"
	"path"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/perfstats"
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
	perfstats.CountSubprocess(tmuxCounterKey(args))
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

// tmuxCounterKey names a tmux invocation by its subcommand for the perfstats
// subprocess table ("tmux list-sessions"), so hot polling paths stand out.
func tmuxCounterKey(args []string) string {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "tmux"
	}
	return "tmux " + args[0]
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

// TmuxHyperlinksFeature is the tmux terminal-feature name for OSC 8 hyperlink
// passthrough. tmux always PARSES OSC 8 out of pane output and keeps the target
// URL in its grid, but it only re-emits the sequence to a client whose terminal
// advertises the `Hls` capability. Neither the terminfo entry for the web
// terminal's TERM nor tmux's own terminal auto-detection (xterm.js answers no
// XTVERSION query) supplies it, so without an explicit opt-in every OSC 8 link
// a harness draws reaches the client as plain, unclickable label text.
const TmuxHyperlinksFeature = "hyperlinks"

// TmuxSyncFeature is the tmux terminal-feature name for synchronized output
// (DEC mode 2026). With it tmux brackets each redraw so the renderer paints the
// finished frame; without it a redraw split across network frames shows its
// intermediate state, most visibly as a flickering cursor. Terminals that do not
// know the mode ignore it.
const TmuxSyncFeature = "sync"

// TmuxClientFeaturesEnv asks a `tclaude session attach` child process to pass
// tmux `-T <features>` for the client it forks, overriding the TERM-based choice
// NativeTmuxClientFeatureArgs makes. The dashboard's web terminals reach tmux
// through that wrapper rather than by spawning tmux themselves, so the "this
// client renders OSC 8" fact has to survive one process hop. It is deliberately
// an env var and not a CLI flag: the native-terminal attach path shares the same
// command builder, and only the caller that knows the renderer may override it.
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

// tmuxHyperlinkTerms are the TERM patterns of a native terminal that gets OSC 8
// from tmux. The opt-in is per client rather than a server-wide terminal-features
// entry on purpose: that entry would also match the federation pane renderer,
// which runs as TERM=xterm-256color, and start sending peers the link targets of
// a shared pane, which are not on its screen.
//
// It is an allowlist because the failure modes differ. A terminal that merely
// ignores OSC 8 loses nothing, but one that prints unknown sequences, like an old
// Linux console, would show the URLs as garbage. tmux*/screen* cover a nested
// client, whose outer multiplexer applies its own policy.
var tmuxHyperlinkTerms = []string{"xterm*", "alacritty*", "foot*", "wezterm*", "ghostty*", "tmux*", "screen*"}

// NativeTmuxClientFeatureArgs returns the tmux client flags for attaching from a
// real terminal identified by term, or nil when it gets tmux's default feature
// set. It is what makes a labelled link a harness draws openable there: without
// the feature tmux drops the target and leaves dead label text, which is worse
// than running the harness outside tmux, where it reaches the same terminal.
//
// The version gate is load-bearing: tmux before 3.2 has no -T and refuses to
// start the client, so an unguarded flag would break every attach, and the
// feature itself arrived in 3.4. Anything that does not parse as a version, like
// a `next-3.5` build, keeps the safe default.
func NativeTmuxClientFeatureArgs(term string) []string {
	if !slices.ContainsFunc(tmuxHyperlinkTerms, func(pattern string) bool {
		matched, err := path.Match(pattern, term)
		return err == nil && matched
	}) {
		return nil
	}
	version, err := Default.Command("-V").Output()
	if err != nil || !TmuxVersionAtLeast(string(version), 3, 4) {
		return nil
	}
	return TmuxClientFeatureArgs(TmuxHyperlinksFeature)
}

// TmuxVersionAtLeast reports whether version, the output of `tmux -V`, names at
// least major.minor. Output it cannot parse reports false.
func TmuxVersionAtLeast(version string, major, minor int) bool {
	var gotMajor, gotMinor int
	if _, err := fmt.Sscanf(strings.TrimSpace(version), "tmux %d.%d", &gotMajor, &gotMinor); err != nil {
		return false
	}
	return gotMajor > major || gotMajor == major && gotMinor >= minor
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
