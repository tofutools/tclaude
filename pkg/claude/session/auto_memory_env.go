package session

import (
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ApplyAutoMemoryEnv pins the spawned/resumed Claude Code pane's auto-memory
// posture by setting CLAUDE_CODE_DISABLE_AUTO_MEMORY in env.
//
// Why tclaude steers this at all: Claude Code's auto-memory system accumulates
// per-project memory files on its own. Under tclaude a single repo routinely
// has several agents running at once, and they all share that one store — so
// one agent's working notes leak into every other agent's context, and a
// worker inherits assumptions nobody asked it to hold. The memory system also
// duplicates what tclaude already tracks per conversation. tclaude therefore
// recommends auto memory OFF and resolves an unset profile field to off; an
// operator who wants it can still opt in per profile or per spawn.
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
