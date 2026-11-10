package config

// TmuxConfig configures how agentd talks to tclaude's tmux server.
type TmuxConfig struct {
	// ControlMode routes agentd's bulk tmux queries (session liveness) over
	// one long-lived `tmux -C` control-mode connection instead of one tmux
	// process per query. Nil means on; false keeps every query on the exec
	// path. The TCLAUDE_TMUX_CONTROL_MODE environment variable, when set,
	// overrides this. Read at agentd startup.
	ControlMode *bool `json:"control_mode,omitempty"`
}

// TmuxControlModeEnabled reports the config's control-mode choice; a nil
// config or unset key means on.
func (c *Config) TmuxControlModeEnabled() bool {
	if c == nil || c.Tmux == nil || c.Tmux.ControlMode == nil {
		return true
	}
	return *c.Tmux.ControlMode
}
