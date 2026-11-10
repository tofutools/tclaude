package session

import (
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ApplyAgentViewEnv makes "off" the default for Claude Code's agent view in a
// pane tclaude launches. See harness.AgentViewDisableEnvVar for why. It writes
// the value unconditionally so an inherited "0" cannot turn the view back on. A
// no-op for any harness without an agent view.
//
// The operator's way back is the `agent-view` context feature. Call this BEFORE
// ApplyContextFeaturesEnv, so an explicit agent-view=on clears the variable: the
// selector must not show "on" for a choice tclaude silently ignores.
//
// `tclaude task run` launches claude directly and sets the variable in
// taskClaudeEnv.
func ApplyAgentViewEnv(h *harness.Harness, env map[string]string) {
	if env == nil || !h.SupportsAgentView() {
		return
	}
	env[harness.AgentViewDisableEnvVar] = "1"
}
