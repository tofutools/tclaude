package session

import (
	"os"

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
// The direct path builds this with LocalHookAmbient, which reads exactly
// the same variables the call sites read before, in the same way — so
// `harness-builtin` behaviour is unchanged. The brokered path fills it in
// agentd from server-authoritative facts where they exist (see
// BrokeredHookAmbient) and from explicitly-carried launch state where
// they do not.
type HookAmbient struct {
	// HarnessPID is the harness process the hook fired under, used to
	// correct a session row still keyed by tmux's pane-shell pid. 0 means
	// "unknown, leave the row's pid alone".
	HarnessPID int

	// TmuxSession is the pane that receives hook-driven pane input (the
	// /clear title restore and the context nudge). "" disables those
	// injections, which is the pre-existing not-in-tmux behaviour.
	TmuxSession string

	// FallbackCwd is used only when a hook payload carries no cwd of its
	// own, during auto-registration of a session tclaude did not launch.
	FallbackCwd string

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
	// This is deliberately EMPTY on the brokered path — see
	// BrokeredHookAmbient.
	TaskSignalPath string
}

// LocalHookAmbient captures the ambient context of the current process.
// This is the direct (non-brokered) path: the hook callback runs as a
// child of the harness, inside the agent's own pane, with the launch
// environment inherited, so reading it here is reading the truth.
func LocalHookAmbient() HookAmbient {
	amb := HookAmbient{
		HarnessPID:        FindClaudePID(),
		TmuxSession:       GetCurrentTmuxSession(),
		ExitGeneration:    os.Getenv("TCLAUDE_EXIT_GENERATION"),
		AutoCompactWindow: os.Getenv(harness.AutoCompactWindowEnvVar),
	}
	if path, ok := taskSignalPath(); ok {
		amb.TaskSignalPath = path
	}
	amb.FallbackCwd, _ = os.Getwd()
	return amb
}

// InTaskRunnerHook reports whether this ambient context belongs to a
// `tclaude task run` hook. See the long note on the task-mode exemptions
// at inTaskRunnerHook's original call sites: the runner is a SEQUENCE of
// independent conversations under one env-session, so the conv-id guards
// have to stand down for it.
func (a HookAmbient) InTaskRunnerHook() bool { return a.TaskSignalPath != "" }

// BrokeredHookAmbient builds the ambient context for a hook event that
// arrived over the agentd broker instead of running in the agent's own
// pane. row is the session row agentd resolved from the caller's recorded
// host pids — never from anything the caller said — and harnessPID is the
// harness ancestor the same walk crossed.
//
// Three of the five fields therefore come out STRONGER than on the direct
// path: the pane, the pid and the fallback cwd are read off state the
// daemon itself recorded at spawn, rather than off a $TMUX the caller
// could have rewritten.
//
// The remaining two are genuine launch-environment values that only the
// caller holds, so they are carried as claims:
//
//   - exitGeneration is compared against the row's own recorded generation
//     precisely to detect a stale observation; substituting the row's value
//     would defeat the check it feeds. A forged value can at most cause the
//     caller's own SessionEnd observation to be accepted or rejected.
//   - autoCompactWindow is parsed, never used raw, so an out-of-range value
//     cannot govern the PreCompact guard.
//
// TaskSignalPath is deliberately left empty and is NOT accepted from the
// caller. It selects a host-side file write whose path and contents would
// both become sandbox-influenceable, and it is unreachable anyway:
// `tclaude task run` sets TCLAUDE_IGNORE_HOOKS on every harness it spawns,
// so a task-runner hook never reaches the broker. Task-mode guard
// exemptions consequently stay OFF for brokered events, which is the
// stricter of the two behaviours.
func BrokeredHookAmbient(src BrokeredHookContext) HookAmbient {
	return HookAmbient{
		HarnessPID:        src.HarnessPID,
		TmuxSession:       src.RowTmuxSession,
		FallbackCwd:       src.RowCwd,
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
