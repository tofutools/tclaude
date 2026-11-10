package config

// ServiceConfig is how agentd runs as a service on this node.
type ServiceConfig struct {
	// Launch is the daemon's launch profile; nil keeps the observed launch.
	Launch *LaunchProfile `json:"launch,omitempty"`
	// Launcher is the app launcher's own setting.
	Launcher *AppLauncherConfig `json:"launcher,omitempty"`
}

// AppLauncherConfig is the app launcher's setting.
type AppLauncherConfig struct {
	// ServeArgs are the `agentd serve` flags the launcher starts agentd with
	// when no launch profile is set; nil means tclaude's defaults.
	ServeArgs []string `json:"serve_args"`
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

// AppLauncherServeArgs returns the launcher's own serve flags, or nil when
// none are set (tclaude's defaults apply).
func (c *Config) AppLauncherServeArgs() []string {
	if c == nil || c.Service == nil || c.Service.Launcher == nil {
		return nil
	}
	return c.Service.Launcher.ServeArgs
}
