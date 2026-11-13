package common

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/perfstats"
)

// TmuxSocketName is the named socket for tclaude's independent tmux server.
const TmuxSocketName = "tclaude"

// Tmux is how a tmux command reaches a tmux server and how its reply comes
// back: the transport under the typed verbs of package tmuxcmd. The default
// LiveTmux forks the local tmux client; agentd swaps in ControlTmux, flow
// tests the simulator, and a runner on another host will provide its own.
//
// Call sites do not use it directly. They go through tmuxcmd, which owns what
// is said to tmux; this interface only carries it. The one caller of Run and
// AttachArgv outside this package is tmuxcmd, and a guard test keeps it so.
type Tmux interface {
	// Run carries one tmux command to the server and returns what it printed
	// on stdout. args is the command as the tmux client takes it after the
	// socket flags: the command word and its arguments, preceded by any
	// client-level flag (-N, -T …).
	//
	// A command tmux ran and rejected fails with *TmuxCommandError carrying
	// tmux's message. A command stopped because ctx ended fails with
	// *TmuxAbortedError, and its outcome is unknown. Any other error means
	// the command could not be delivered.
	Run(ctx context.Context, args []string) (stdout []byte, err error)
	// AttachArgv returns the argv of a process that, given a terminal, runs
	// the tmux client with args (an attach-session). It is the one
	// exec-shaped part of the seam: attaching is a long-lived interactive
	// process that owns a TTY, not a command with a reply, so the caller has
	// to start it on a terminal it holds.
	AttachArgv(args []string) ([]string, error)
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

// Default is the package-wide Tmux instance every tmuxcmd verb goes through.
// Production starts on LiveTmux; tests overwrite it during their setup.
// Single global var = goroutine-unsafe across parallel tests on the same
// package — flow tests don't t.Parallel.
var Default Tmux = LiveTmux{}

// LiveTmux is the production impl: forks `tmux -u -L tclaude <args>`.
// Exported so tests can wrap it (e.g., a recording proxy that
// forwards to LiveTmux for some calls and to a fake for others).
type LiveTmux struct{}

// Run forks the tmux client for one command.
func (LiveTmux) Run(ctx context.Context, args []string) ([]byte, error) {
	perfstats.CountSubprocess(tmuxCounterKey(args))
	return RunTmuxClient(ctx, exec.Command("tmux", TmuxArgs(args...)...))
}

// AttachArgv is the local tmux client itself.
func (LiveTmux) AttachArgv(args []string) ([]string, error) {
	perfstats.CountSubprocess(tmuxCounterKey(args))
	path, err := exec.LookPath("tmux")
	if err != nil {
		return nil, err
	}
	return append([]string{path}, TmuxArgs(args...)...), nil
}

// tmuxClientWaitDelay bounds how long a tmux client that has exited or been
// killed is waited for when something it started still holds its output
// pipes open. A variable so a test can shorten it.
var tmuxClientWaitDelay = 2 * time.Second

// RunTmuxClient runs a prepared tmux client process to completion for a Tmux
// implementation and reports its outcome in the seam's error model: stdout
// on success, *TmuxCommandError (with tmux's stderr) when the client exits
// non-zero, *TmuxAbortedError when ctx ends first. A client stopped by ctx is
// killed and reaped before this returns, so a caller can release what it
// holds knowing no child of its own is still running.
func RunTmuxClient(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = tmuxClientWaitDelay
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		// A client that exited cleanly has answered, even if something it
		// started kept its output pipes open past the wait delay.
		if err == nil || errors.Is(err, exec.ErrWaitDelay) {
			return stdout.Bytes(), nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, NewTmuxExitError(exitErr, stderr.String())
		}
		return nil, err
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return nil, &TmuxAbortedError{Cause: context.Cause(ctx), Stderr: strings.TrimSpace(stderr.String())}
	}
}

// TmuxAbortedError is a command that was stopped because its context ended
// before tmux answered. Whether tmux acted on it is unknown.
type TmuxAbortedError struct {
	// Cause is why the context ended.
	Cause error
	// Stderr is what the tmux client had printed by then, if anything.
	Stderr string
}

func (e *TmuxAbortedError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("tmux command stopped: %v: %s", e.Cause, e.Stderr)
	}
	return fmt.Sprintf("tmux command stopped: %v", e.Cause)
}

func (e *TmuxAbortedError) Unwrap() error { return e.Cause }

// tmuxListWarned holds the tmux failures already logged, so a poll loop
// logs each distinct one once.
var tmuxListWarned sync.Map

// warnTmuxListFailure logs why listing tclaude's tmux sessions failed,
// unless it is only that no server runs. Every agent's liveness comes from
// this listing, so any other failure (tmux missing, a socket directory
// with the wrong owner or mode, a server of another tmux version) shows
// every agent offline; without this line nothing said why.
func warnTmuxListFailure(msg string) {
	msg = strings.TrimSpace(msg)
	if tmuxNoServer(msg) {
		return
	}
	if _, seen := tmuxListWarned.LoadOrStore(msg, true); seen {
		return
	}
	slog.Warn("tmux: listing tclaude's sessions failed; every agent shows offline", "error", msg)
}

// tmuxNoServer reports whether a failed `tmux ls` said only that no server
// runs: tmux prints "no server running on <socket>", or "error connecting
// to <socket> (No such file or directory)" when the socket does not exist.
func tmuxNoServer(msg string) bool {
	return strings.HasPrefix(msg, "no server running on ") ||
		(strings.HasPrefix(msg, "error connecting to ") && strings.HasSuffix(msg, "(No such file or directory)"))
}

// TmuxNoServerMessage reports whether msg, tmux's error text for a failed
// command, says only that no server is running.
func TmuxNoServerMessage(msg string) bool { return tmuxNoServer(strings.TrimSpace(msg)) }

// tmuxCounterKey names a tmux invocation by its subcommand for the perfstats
// subprocess table ("tmux list-sessions"), so hot polling paths stand out.
func tmuxCounterKey(args []string) string {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "tmux"
	}
	return "tmux " + args[0]
}

// TmuxUTF8Flag makes a tmux client handle its I/O as UTF-8 whatever its
// locale. Without it, a client whose LC_ALL / LC_CTYPE / LANG names no UTF-8
// locale, and which is not itself inside tmux, escapes control characters
// in what it prints. agentd under launchd has no locale, so the tab in
// `list-sessions -F '#{session_name}\t#{pane_dead}'` came out as '_', no
// session name matched, and every agent read as offline. Every tclaude tmux
// client passes it, including commands built as shell text.
const TmuxUTF8Flag = "-u"

// TmuxArgs prepends -u -L tclaude to the given tmux arguments.
func TmuxArgs(args ...string) []string {
	return append([]string{TmuxUTF8Flag, "-L", TmuxSocketName}, args...)
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
	version, err := Default.Run(context.Background(), []string{"-V"})
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
