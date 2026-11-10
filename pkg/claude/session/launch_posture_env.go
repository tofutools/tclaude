package session

import "github.com/tofutools/tclaude/pkg/claude/harness"

// LaunchPostureEnv is the part of a launch's recorded posture that reaches
// the pane through its environment. A fresh spawn resolves it from the launch
// request; a resume reads it back off the conversation's record, so the
// resumed pane is the same agent rather than one re-defaulted from today's
// settings.
type LaunchPostureEnv struct {
	AutoMemory        bool
	ContextFeatures   map[string]string
	AutoCompactWindow string
}

// ApplyLaunchPostureEnv writes the launch-posture environment shared by a
// fresh spawn (NewParams) and a resume (conv resume / watch auto-resume), in
// the one order both must use. Each step is a no-op for a harness without the
// feature; see the individual Apply*Env functions for what they set.
//
// The order is load-bearing: ApplyAgentViewEnv defaults the agent view off
// and must run before ApplyContextFeaturesEnv, which is how an explicit
// agent-view=on opts back in.
func ApplyLaunchPostureEnv(h *harness.Harness, posture LaunchPostureEnv, env map[string]string) error {
	// Every tclaude-launched Claude pane reads its config from the state root.
	if err := ApplyClaudeConfigDirEnv(h.Name, env); err != nil {
		return err
	}
	// Keep Claude Code's "Resume from summary" chooser from blocking a
	// detached pane.
	ApplyClaudeResumeEnv(h, env)
	// Unset resolves to OFF: agents sharing a repo would otherwise share one
	// per-project memory store.
	ApplyAutoMemoryEnv(h, posture.AutoMemory, env)
	ApplyAgentViewEnv(h, env)
	// Sparse and opt-in: an empty map injects nothing.
	ApplyContextFeaturesEnv(h, posture.ContextFeatures, env)
	// Unset injects nothing and the model's own threshold decides.
	ApplyAutoCompactWindowEnv(h, posture.AutoCompactWindow, env)
	return nil
}
