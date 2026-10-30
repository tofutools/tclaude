package session

import (
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ApplyAgentViewEnv turns off Claude Code's agent view for a pane tclaude
// launches. See harness.AgentViewDisableEnvVar for why a managed pane cannot
// have it.
//
// It has no opt-in, unlike ApplyAutoMemoryEnv: the view does not trade one
// posture for another, it breaks tclaude's tracking of the session. It writes the value unconditionally so an inherited "0" cannot
// turn the view back on. A no-op for any harness without an agent view.
//
// `tclaude task run` launches claude directly and does the same in taskClaudeEnv.
func ApplyAgentViewEnv(h *harness.Harness, env map[string]string) {
	if env == nil || !h.SupportsAgentView() {
		return
	}
	env[harness.AgentViewDisableEnvVar] = "1"
}
