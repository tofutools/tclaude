package harness

import "fmt"

// AutoMemoryEnvVar is Claude Code's auto-memory switch. Claude Code documents
// it as a tri-value knob read from the environment: "1" disables auto memory,
// "0" force-ENABLES it (overriding a disable that came from elsewhere), and an
// absent variable leaves Claude Code on its own default. tclaude always sets
// one of the two explicit values for a Claude Code launch, so a stray "=1"
// exported in the operator's own shell can never silently decide an agent's
// posture.
//
// It governs only the auto-generated memory store — the notes Claude Code
// accumulates about a project on its own. CLAUDE.md / AGENTS.md are unaffected.
const AutoMemoryEnvVar = "CLAUDE_CODE_DISABLE_AUTO_MEMORY"

// AutoMemoryEnvValue renders the value AutoMemoryEnvVar must carry for the
// given posture. Note the inversion: the variable disables, so auto memory ON
// is "0". Callers should route through this rather than open-coding the
// literals, precisely because that inversion is easy to get backwards.
func AutoMemoryEnvValue(autoMemory bool) string {
	if autoMemory {
		return "0"
	}
	return "1"
}

// SupportsAutoMemory reports whether the harness has an auto-memory system
// tclaude can steer. This is Claude Code's feature; Codex has no equivalent
// store and no corresponding switch, so callers must not emit the env var for
// it — and must hide the affordance.
//
// Gated on the harness NAME rather than a capability func because the switch
// is a plain environment variable, not a lifecycle command with a per-harness
// implementation to probe (the same reasoning as session.ApplyClaudeResumeEnv).
func (h *Harness) SupportsAutoMemory() bool {
	return h != nil && h.Name == DefaultName
}

// CanAutoMemory is the UI-side predicate a spawn/profile control gates on
// (mirrors CanRemoteControl). Identical to SupportsAutoMemory today; kept
// separate so a future "supported but not steerable here" case has a seam.
func (h *Harness) CanAutoMemory() bool {
	return h.SupportsAutoMemory()
}

// ResolveAutoMemory gates the "let Claude Code keep its auto-memory files"
// opt-in and returns the posture to thread into the launch.
//
// Unlike ResolveRemoteControl there IS a meaningful non-zero default: tclaude
// recommends auto memory OFF, because several tclaude agents working the same
// repo share one per-project memory store and cross-pollute each other's
// notes. So an unset profile field (nil) resolves to false, and the caller
// injects the disable. Requesting auto memory ON for a harness that has no
// auto-memory system is an error rather than a silent drop, so a mistake
// surfaces at the spawn boundary instead of at runtime.
//
// One function serves both the daemon spawn path and the direct `session new`
// path.
func ResolveAutoMemory(h *Harness, requested *bool) (bool, error) {
	if requested == nil {
		return false, nil
	}
	if *requested && !h.CanAutoMemory() {
		return false, fmt.Errorf("harness %q has no auto-memory system "+
			"(auto memory is a Claude Code feature; not available for this harness)", harnessName(h))
	}
	return *requested, nil
}

func harnessName(h *Harness) string {
	if h == nil {
		return ""
	}
	return h.Name
}
