package session

import (
	"os"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// HookAmbient carries the per-launch process context the hook callback
// would otherwise read straight out of its own environment: the harness
// pid it runs under, the tmux pane it lives in, the launch generation
// token, the pinned auto-compact window, and the task-runner signal path.
//
// It exists because TCL-754 brokers hook events through agentd for
// `tclaude-layer` launches. Every one of those values is ambient state of
// the process that *fires* the hook, and agentd is not that process: its
// $TMUX is unset, its parents are not harnesses, and its environment
// carries none of the launch variables. Read ambiently on the daemon side
// they silently resolve to the daemon's own (wrong, mostly empty) context
// — the row would lose its pane, its pid correction, and its compaction
// pin. Threading them explicitly makes the difference between the two
// paths a value, not a hidden environmental dependency.
//
// The direct path builds this with LocalHookAmbient; the brokered path
// with BrokeredHookAmbient, from server-authoritative facts where they
// exist and explicitly-carried launch state where they do not.
type HookAmbient struct {
	// harnessPID and tmuxSession are resolved LAZILY and at most once.
	// That is load-bearing, not a micro-optimisation: GetCurrentTmuxSession
	// execs `tmux display-message` and FindClaudePID walks /proc, and
	// before this seam existed both ran only on the two rare paths that
	// actually needed them (auto-registration and the context nudge).
	// Resolving them eagerly for every event would put a subprocess spawn
	// on the critical path of every PreToolUse/PostToolUse of every
	// harness-builtin agent — a behaviour change to the launch mode that
	// is required not to change.
	harnessPID  func() int
	tmuxSession func() string

	// fallbackCwd is likewise lazy, and consulted only when a hook payload
	// carries no cwd of its own during auto-registration.
	fallbackCwd func() string

	// ExitGeneration is TCLAUDE_EXIT_GENERATION — the per-launch token
	// that lets a SessionEnd observation be rejected as stale when it
	// belongs to a previous launch of the same session.
	ExitGeneration string

	// AutoCompactWindow is the raw TCLAUDE_AUTO_COMPACT_WINDOW value. It
	// is parsed (never used raw) by the PreCompact guard, so an
	// out-of-range value cannot govern the decision.
	AutoCompactWindow string

	// TaskSignalPath is the `tclaude task run` signal file, already bounded
	// to CacheDir by taskSignalPath. A non-empty value means task mode,
	// which also relaxes several hook guards.
	//
	// This is always EMPTY on the brokered path — see BrokeredHookAmbient.
	TaskSignalPath string
}

// HarnessPID is the harness process the hook fired under, used to correct
// a session row still keyed by tmux's pane-shell pid. 0 means "unknown,
// leave the row's pid alone".
func (a HookAmbient) HarnessPID() int {
	if a.harnessPID == nil {
		return 0
	}
	return a.harnessPID()
}

// TmuxSession is the pane that receives hook-driven pane input (the
// /clear title restore and the context nudge). "" disables those
// injections, which is the pre-existing not-in-tmux behaviour.
func (a HookAmbient) TmuxSession() string {
	if a.tmuxSession == nil {
		return ""
	}
	return a.tmuxSession()
}

// FallbackCwd is used only when a hook payload carries no cwd of its own,
// during auto-registration of a session tclaude did not launch.
func (a HookAmbient) FallbackCwd() string {
	if a.fallbackCwd == nil {
		return ""
	}
	return a.fallbackCwd()
}

// InTaskRunnerHook reports whether this ambient context belongs to a
// `tclaude task run` hook. See the note on the task-mode exemptions at
// hook_callback.go: the runner is a SEQUENCE of independent conversations
// under one env-session, so the conv-id guards have to stand down for it.
func (a HookAmbient) InTaskRunnerHook() bool { return a.TaskSignalPath != "" }

// LocalHookAmbient captures the ambient context of the current process.
// This is the direct (non-brokered) path: the hook callback runs as a
// child of the harness, inside the agent's own pane, with the launch
// environment inherited, so reading it here is reading the truth.
//
// The environment variables are read eagerly (they are free); the pid
// walk, the tmux probe and the getcwd are deferred to first use, so a
// hook that never needs them never pays for them — which is what keeps
// this identical to the inline reads it replaced.
func LocalHookAmbient() HookAmbient {
	amb := HookAmbient{
		harnessPID:        sync.OnceValue(FindClaudePID),
		tmuxSession:       sync.OnceValue(GetCurrentTmuxSession),
		fallbackCwd:       sync.OnceValue(func() string { cwd, _ := os.Getwd(); return cwd }),
		ExitGeneration:    os.Getenv("TCLAUDE_EXIT_GENERATION"),
		AutoCompactWindow: os.Getenv(harness.AutoCompactWindowEnvVar),
	}
	if path, ok := taskSignalPath(); ok {
		amb.TaskSignalPath = path
	}
	return amb
}

// BrokeredHookAmbient builds the ambient context for a hook event that
// arrived over the agentd broker instead of running in the agent's own
// pane. The Row-prefixed fields of src must come off the session row
// agentd resolved from the caller's recorded host pids — never from
// anything the caller said — and HarnessPID is the harness ancestor the
// same walk crossed.
//
// Three of the five values therefore come out STRONGER than on the direct
// path: the pane, the pid and the fallback cwd are read off state the
// daemon itself recorded at spawn, rather than off a $TMUX the caller
// could have rewritten.
//
// The remaining two are genuine launch-environment values that only the
// caller holds, so they are carried as claims:
//
//   - ExitGeneration is compared against the row's own recorded generation
//     precisely to detect a stale observation; substituting the row's value
//     would defeat the check it feeds. A forged value can at most cause the
//     caller's own SessionEnd observation to be accepted or rejected.
//   - AutoCompactWindow is parsed, never used raw, so an out-of-range value
//     cannot govern the PreCompact guard.
//
// TaskSignalPath is deliberately left empty and is NOT accepted from the
// caller: it selects a host-side file write whose path and contents would
// both become sandbox-influenceable. The client refuses to broker at all
// while a task signal is set (see brokerHookEvents), so this is a second
// line rather than the only one. Task-mode guard exemptions consequently
// stay OFF for brokered events, which is the stricter of the two
// behaviours.
func BrokeredHookAmbient(src BrokeredHookContext) HookAmbient {
	return HookAmbient{
		harnessPID:        func() int { return src.HarnessPID },
		tmuxSession:       func() string { return src.RowTmuxSession },
		fallbackCwd:       func() string { return src.RowCwd },
		ExitGeneration:    src.ExitGeneration,
		AutoCompactWindow: src.AutoCompactWindow,
	}
}

// BrokeredHookContext is what agentd hands BrokeredHookAmbient. The
// Row-prefixed fields must come off the session row the daemon resolved
// from recorded host pids; the other two are the caller's carried launch
// environment. The naming is the reminder: mixing the two sources up is
// the one way to turn this into an impersonation surface.
type BrokeredHookContext struct {
	RowTmuxSession    string
	RowCwd            string
	HarnessPID        int
	ExitGeneration    string
	AutoCompactWindow string
}
