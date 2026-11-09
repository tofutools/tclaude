package config

// ServiceConfig is how agentd runs as a service on this node.
type ServiceConfig struct {
	// Launch is the daemon's launch profile; nil keeps the observed launch.
	Launch *LaunchProfile `json:"launch,omitempty"`
}

// LaunchProfile is the `agentd serve` launch tclaude writes into the
// autostart unit, and expects the daemon to run with. It never holds a
// secret value: secret-looking variables are inherited by name, their value
// read from the running daemon's environment when the unit is written, and
// secret-looking flags are refused.
type LaunchProfile struct {
	// Flags are `agentd serve` flags, one token each (`--name` or
	// `--name=value`).
	Flags []string `json:"flags"`
	// Env are explicit, non-secret KEY=VALUE entries.
	Env []LaunchEnv `json:"env"`
	// InheritEnv names variables whose value is taken from the running
	// daemon's environment when the unit is written.
	InheritEnv []string `json:"inherit_env"`
}

// LaunchEnv is one explicit environment variable of a LaunchProfile.
type LaunchEnv struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ServiceLaunch returns the configured launch profile, or nil.
func (c *Config) ServiceLaunch() *LaunchProfile {
	if c == nil || c.Service == nil {
		return nil
	}
	return c.Service.Launch
}
