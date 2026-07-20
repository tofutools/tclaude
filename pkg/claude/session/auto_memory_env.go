package session

import (
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ApplyAutoMemoryEnv pins the spawned/resumed Claude Code pane's auto-memory
// posture by setting CLAUDE_CODE_DISABLE_AUTO_MEMORY in env.
//
// Why tclaude steers this at all: Claude Code's auto-memory system accumulates
// memory files on its own, keyed by project directory. Under tclaude a single
// checkout routinely has several agents running at once, and every agent in
// that directory shares the one store — so one agent's working notes leak into
// every other agent's context, and a worker inherits assumptions nobody asked
// it to hold. (Agents in separate git worktrees get separate stores, so the
// collision is worst exactly where agents share a checkout.) tclaude therefore
// recommends auto memory OFF and resolves an unset field to off; an operator
// who wants it can still opt in per profile, per spawn, or per session.
//
// Note this applies to EVERY Claude Code session tclaude launches, including a
// plain interactive `tclaude session new` with no agents involved — the
// injection lives on the shared launch path. `--auto-memory` opts a single
// session back in.
//
// The variable is set EXPLICITLY in both directions ("1" = off, "0" = force
// on) rather than merely omitted for the on case. BuildEnvExports forwards the
// operator's own os.Environ(), so an operator who exports
// CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 in their shell would otherwise silently
// override an agent that opted into memory. Writing the value unconditionally
// makes the resolved posture authoritative.
//
// A no-op for any harness without an auto-memory system (Codex), so the two
// call sites stay simple. It is the single seam both env-assembly paths route
// through: session.runNew (spawn and `tclaude session new -r` resume) and
// conv.resumeLaunchCmd (watch-mode resume) — the sibling of
// ApplyClaudeResumeEnv.
func ApplyAutoMemoryEnv(h *harness.Harness, autoMemory bool, env map[string]string) {
	if env == nil || !h.SupportsAutoMemory() {
		return
	}
	env[harness.AutoMemoryEnvVar] = harness.AutoMemoryEnvValue(autoMemory)
}
