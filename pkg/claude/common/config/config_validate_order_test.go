package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validate reports every problem at once, in a fixed order the dashboard's
// config editor shows them in. One bad value per section pins that order and
// each message's wording while Validate is reshaped into per-section steps.
// Each section here yields one message, so map iteration cannot reorder them.
func TestValidate_ReportsEachSectionInOrder(t *testing.T) {
	neg := -1
	big := 101
	bad := "nope"
	factor := -1.0
	c := &Config{
		Ask:            &AskConfig{Sandbox: "profile:"},
		StatusSnapshot: &StatusSnapshotConfig{FreshnessMS: -1},
		Updates:        &UpdatesConfig{Scripts: []UpdateScriptConfig{{Name: "Bad"}}},
		Dashboard: &DashboardConfig{
			Keybindings:    map[string]string{"x": "!!"},
			TerminalAttach: &TerminalAttachConfig{Mode: "bogus"},
		},
		Federation:              &FederationConfig{MaxLiveAgents: -1},
		Capacity:                &CapacityConfig{MaxLiveAgents: -1},
		Host:                    &HostConfig{WorkDirs: []string{"rel"}},
		LogLevel:                "loud",
		PreCompactGuard:         &PreCompactGuardConfig{Thresholds: []PreCompactThreshold{{WindowSize: 0}}},
		RateLimit:               &RateLimitConfig{FiveHourPercentMaxUsed: 50, SevenDayPercentMaxUsed: 0},
		OpenCode:                &OpenCodeConfig{LegacyLongContextPricingCutoff: new(int64)},
		ClaudeCleanupPeriodDays: -1,
		RemoteAccess:            &RemoteAccessConfig{Enabled: true},
		Notifications:           &NotificationConfig{CooldownSeconds: -1},
		Agent: &AgentConfig{
			AWBProxy:              &AWBProxyConfig{ReadyPolling: map[string]AWBReadyPollingConfig{"a": {Workspace: "w"}, "b": {Workspace: "w"}}},
			ResourceDelegationDir: "rel",
			CloneCooldown:         "-1s",
			DashboardPort:         -1,
			ContextNudge:          &ContextNudgeConfig{Enabled: true},
			RetiredCleanup:        &RetiredCleanupConfig{Enabled: true},
			Sudo:                  &SudoConfig{MaxDuration: "x"},
		},
		Cost:         &CostConfig{EstimateFactor: &factor, GeminiBilling: "bogus"},
		Usage:        &UsageConfig{IdleTimeout: "-1h"},
		ClaudeResume: &ClaudeResumeConfig{ThresholdMinutes: &neg},
		Slop:         &SlopConfig{MusicVolume: &big, Channel: &bad},
		LogRotation:  &LogRotationConfig{Keep: -1},
		Focus:        &FocusConfig{Tile: &TileConfig{Layout: "bogus"}},
		Steward:      &StewardConfig{Reads: "bogus"},
		TUI:          &TUIConfig{ColorScheme: "bogus"},
		Features:     &FeaturesConfig{GroupAttachments: "bogus"},
	}
	assert.Equal(t, []string{
		"ask.sandbox: choose auto, global, off, or a valid sandbox profile name: sandbox profile name is required",
		"status_snapshot.freshness_ms must be 0 (default) or 1..60000",
		"updates.scripts: name must be 1-40 lowercase letters, digits, - or _, starting with a letter or digit",
		"dashboard.keybindings: \"x\" → \"!!\" is not a shortcut id and a chord like Ctrl+Alt+KeyK",
		"federation.max_live_agents must be nonnegative (zero is unlimited)",
		"capacity.max_live_agents must be nonnegative (zero is unlimited)",
		"host.work_dirs must contain absolute paths",
		"log_level \"loud\" is not one of debug, info, warn, error",
		"pre_compact_guard.thresholds[0].window_size must be positive",
		"ratelimit.seven_day_percent_max_used 0 is out of range (>0 and ≤100)",
		"opencode.legacy_long_context_pricing_cutoff 0 must be positive (default 272000)",
		"claude_cleanup_period_days -1 must not be negative (0 = leave Claude Code's default; a positive day count overrides it)",
		"remote_access.enabled is set but remote_access.bind is empty; set a bind address (e.g. 0.0.0.0:8443)",
		"notifications.cooldown_seconds must not be negative",
		"agent.awb_proxy.ready_polling processes for workspace \"w\" must have disjoint non-empty label filters",
		"agent.resource_delegation_dir \"rel\" must be an absolute path",
		"agent.clone_cooldown must not be negative",
		"agent.dashboard_port -1 is out of range (1–65535, or 0/absent for a random free port)",
		"agent.context_nudge.min_pct 0 is out of range (1–100)",
		"agent.context_nudge.interval_pct 0 is out of range (1–100)",
		"agent.retired_cleanup.after_days 0 is out of range (must be 1–36525 — it is the number of days an agent stays retired before it is permanently deleted)",
		"agent.sudo.max_duration \"x\" is not a valid duration (e.g. \"30m\", \"2h\")",
		"cost.estimate_factor -1 is out of range (>0 and ≤10) — it is a display multiplier, e.g. 1.1 for +10%",
		"cost.gemini_billing \"bogus\" is not one of \"auto\", \"api\" or \"subscription\"",
		"usage.idle_timeout \"-1h\" must be positive — it is how long the last-known usage reading stays on the dashboard after its source goes idle",
		"claude_resume.threshold_minutes -1 must not be negative (use a large value to suppress the prompt, 0 to always show it)",
		"slop.music_volume 101 is out of range (0–100)",
		"slop.channel \"nope\" is not a known SomaFM channel (one of: illstreet, secretagent, groovesalad, lush, bootliquor, u80s, defcon, thistle, folkfwd, dronezone, darkzone, doomed, deepspaceone)",
		"log_rotation.keep -1 must not be negative (0 = built-in default)",
		"focus.tile.layout \"bogus\" is not one of grid, columns, rows, cascade",
		"dashboard.terminal_attach.mode \"bogus\" is not one of repair, initial, pre_attach",
		"steward.reads \"bogus\" is not one of operator, fleet",
		"tui.color_scheme \"bogus\" is not one of default, dark-high-contrast",
		"features.group_attachments \"bogus\" is not one of off, float, fixed",
	}, Validate(c))
}
