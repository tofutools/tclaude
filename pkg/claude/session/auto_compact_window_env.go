package session

import (
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ApplyAutoCompactWindowEnv pins the spawned/resumed Claude Code pane's
// auto-compaction context capacity by setting CLAUDE_CODE_AUTO_COMPACT_WINDOW
// in env.
//
// Why tclaude steers this at all: on a 1M-context model Claude Code will run
// most of the way to the full window before auto-compacting, and answer quality
// falls off well before that point. Pinning the window lower makes compaction
// fire while the agent is still sharp. See harness.AutoCompactWindowEnvVar for
// the variable's exact semantics — in particular that it is capped at the
// model's real context window, and that it decouples the compaction threshold
// from the status line's percentage.
//
// Unlike ApplyAutoMemoryEnv this writes the variable in ONE direction only.
// There is no documented value meaning "use the model default", so an unset
// window must omit the variable rather than write a sentinel — which also means
// an operator who exports CLAUDE_CODE_AUTO_COMPACT_WINDOW in their own shell
// keeps it for any session that did not choose one. A session that DID choose
// overwrites it, so the resolved posture still wins where it exists.
//
// A no-op for any harness without an auto-compaction window (Codex, OpenCode)
// and for a blank window, so the call sites stay simple. It is the single seam
// both env-assembly paths route through: session.runNew (spawn and `tclaude
// session new -r` resume) and conv.resumeLaunchCmd (watch-mode resume) — the
// sibling of ApplyAutoMemoryEnv and ApplyContextFeaturesEnv.
// The window is re-parsed here rather than trusted, so this seam can never
// inject a value the spawn boundary would have rejected. Callers reach it with a
// window read back out of durable state (a session row, a relaunch profile), and
// that state can predate the bounds, or have been hand-edited. An unparseable or
// out-of-range value is dropped, leaving Claude Code's own default threshold in
// charge — the same fail-soft direction durableRelaunchConfigForConv takes,
// because losing a pin costs an earlier compaction while honouring a bad one
// would wedge the agent's whole context accounting.
func ApplyAutoCompactWindowEnv(h *harness.Harness, window string, env map[string]string) {
	if env == nil || strings.TrimSpace(window) == "" || !h.SupportsAutoCompactWindow() {
		return
	}
	canonical, err := harness.ParseAutoCompactWindow(window)
	if err != nil || canonical == "" {
		return
	}
	env[harness.AutoCompactWindowEnvVar] = canonical
}
