package agentd

import (
	"log/slog"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// tmuxControlModeChoice resolves whether agentd uses tmux control mode, and
// says why: the environment variable wins over the config key, and both
// absent means on.
func tmuxControlModeChoice(cfg *config.Config) (bool, string) {
	if disabled, ok := clcommon.ControlModeDisabledByEnv(); ok {
		if disabled {
			return false, clcommon.ControlModeEnvVar + " turns it off"
		}
		return true, clcommon.ControlModeEnvVar + " turns it on"
	}
	if !cfg.TmuxControlModeEnabled() {
		return false, "config tmux.control_mode is false"
	}
	return true, "default"
}

// startTmuxControlMode installs the control-mode tmux client as
// clcommon.Default for the daemon's lifetime and returns its teardown. It
// must run before any goroutine reads clcommon.Default.
//
// It only replaces the production LiveTmux: a flow test's TmuxSim (or any
// other injected fake) is left in place, so the clcommon.Default seam keeps
// working unchanged.
func startTmuxControlMode(cfg *config.Config) func() {
	on, why := tmuxControlModeChoice(cfg)
	if _, live := clcommon.Default.(clcommon.LiveTmux); !live {
		return func() {}
	}
	if !on {
		slog.Info("tmux control mode: off, every tmux query forks a tmux process", "reason", why, "module", "tmuxctl")
		return func() {}
	}
	ct := clcommon.NewControlTmux()
	clcommon.Default = ct
	slog.Info("tmux control mode: on; connects once a tclaude tmux session exists", "reason", why,
		"switch_off", clcommon.ControlModeEnvVar+"=0 or config tmux.control_mode=false", "module", "tmuxctl")

	// Session set changes arrive as notifications: invalidate the cached
	// liveness snapshot (and the status snapshot keyed on the same
	// generation) at once instead of waiting out the TTL, so a killed or new
	// session shows on the next read.
	notes, cancel := ct.Client().Subscribe()
	go func() {
		for n := range notes {
			switch n.Name {
			case "sessions-changed", "session-closed", "connected", "disconnected":
				db.NotifyStatusChanged()
			}
		}
	}()
	return func() {
		cancel()
		ct.Close()
	}
}
