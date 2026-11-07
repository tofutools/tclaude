// Package config provides configuration loading for tclaude.
package config

// Config represents the tclaude configuration file structure.
type Config struct {
	StatusSnapshot  *StatusSnapshotConfig  `json:"status_snapshot,omitempty"`
	Host            *HostConfig            `json:"host,omitempty"`
	Capacity        *CapacityConfig        `json:"capacity,omitempty"`
	Notifications   *NotificationConfig    `json:"notifications,omitempty"`
	PreCompactGuard *PreCompactGuardConfig `json:"pre_compact_guard,omitempty"`
	LogLevel        string                 `json:"log_level,omitempty"`
	// StartupTiming enables startup diagnostics without restarting agentd.
	// Nil falls back to TCLAUDE_STARTUP_TIMING; explicit false overrides it.
	StartupTiming *bool            `json:"startup_timing,omitempty"`
	RecordHooks   bool             `json:"record_hooks,omitempty"`
	RateLimit     *RateLimitConfig `json:"ratelimit,omitempty"`
	Agent         *AgentConfig     `json:"agent,omitempty"`

	// ExperimentalHarnesses opts in to registered harnesses whose tclaude
	// support is still incomplete (Harness.Experimental; none today). Such a
	// harness cannot be spawned and is left out of spawn surfaces' default
	// lists until it is named here.
	ExperimentalHarnesses []string `json:"experimental_harnesses,omitempty"`

	// Broker configures agentd's brokered hook/statusline endpoints —
	// the path a `tclaude-layer` agent uses to reach the conversation
	// database its mount namespace hides. Absent → limits measured and
	// logged but not enforced. See BrokerConfig.
	Broker *BrokerConfig `json:"broker,omitempty"`

	// Terminal names the terminal emulator the agentd dashboard's
	// spawn auto-focus / shell-attach feature should open — "ghostty",
	// "kitty", "wezterm", "alacritty", "foot", "iterm2", "konsole",
	// "gnome-terminal", … .
	// Empty means auto-detect, which prefers a hand-installed modern
	// terminal over the OS default. This is the middle tier of the
	// terminal-selection priority: the `tclaude agentd serve
	// --terminal` flag overrides it; auto-detect is the fallback.
	Terminal string `json:"terminal,omitempty"`

	// LogRotation configures size-based rotation of ~/.tclaude/output.log,
	// performed by the agentd daemon. Absent block / absent keys fall
	// back to the built-in defaults — see ResolvedLogRotation.
	LogRotation *LogRotationConfig `json:"log_rotation,omitempty"`

	// Focus configures window-focus behavior. Absent → defaults (focus
	// raises an existing window and opens a fresh one when none is open).
	Focus *FocusConfig `json:"focus,omitempty"`

	// Slop holds the dashboard's slop-mode ("The Slop Machine") audio
	// volumes. An absent block / absent keys default the music to half
	// volume and the effects to full — see ResolvedSlopVolumes.
	Slop *SlopConfig `json:"slop,omitempty"`

	// ConvWatch holds persisted UI preferences for the interactive
	// `tclaude conv ls -w` watch view. Absent → all defaults.
	ConvWatch *ConvWatchConfig `json:"conv_watch,omitempty"`

	// SessionWatch holds persisted UI preferences for the interactive
	// `tclaude sessions` watch view. Absent → all defaults.
	SessionWatch *SessionWatchConfig `json:"session_watch,omitempty"`

	// Cost holds display-only cost adjustments — see CostConfig. Absent →
	// no adjustment (the recorded figures are shown verbatim).
	Cost *CostConfig `json:"cost,omitempty"`

	// OpenCode holds tclaude-side OpenCode integration settings. These values
	// tune tclaude's projection of OpenCode data; they are not written into
	// OpenCode's own configuration.
	OpenCode *OpenCodeConfig `json:"opencode,omitempty"`

	// Ask holds the default model/effort profile for `tclaude ask` — see
	// AskConfig. Absent / blank fields fall back to the built-in default
	// constants (DefaultAskModel + DefaultAskEffort); see
	// ResolvedAskProfile.
	Ask *AskConfig `json:"ask,omitempty"`

	// Scribe holds the launch profile for dashboard-summoned scribes — see
	// ScribeConfig. Absent / blank = today's behaviour (the harness default:
	// Claude Code at its default model/effort); see ScribeProfileName.
	Scribe *ScribeConfig `json:"scribe,omitempty"`

	// Steward sets what a freshly summoned Steward may do — see StewardConfig.
	Steward *StewardConfig `json:"steward,omitempty"`

	// Audit holds the audit-log retention policy (JOH-268). Absent block /
	// absent keys fall back to the built-in default — see
	// ResolvedAuditRetentionDays.
	Audit *AuditConfig `json:"audit,omitempty"`

	// RemoteAccess configures the optional network-exposed dashboard
	// listener — see RemoteAccessConfig. Absent / disabled (the default)
	// keeps agentd loopback-only.
	RemoteAccess *RemoteAccessConfig `json:"remote_access,omitempty"`

	// Federation links this agentd to other instances through a
	// tclaude-hub. Absent / disabled → no hub connection. See
	// FederationConfig and docs/federation.md.
	Federation *FederationConfig `json:"federation,omitempty"`

	// Updates holds this node's own update scripts and harness update
	// overrides — see UpdatesConfig and docs/federation.md. Only the local
	// operator sets them: they are shell commands agentd runs, so they never
	// travel in config bundles or peer requests.
	Updates *UpdatesConfig `json:"updates,omitempty"`

	// Snapshots configures the local agent snapshot library.
	Snapshots *SnapshotsConfig `json:"snapshots,omitempty"`

	// Routes tunes the group-route data plane — see RoutesConfig. Absent →
	// defaults (flow control on).
	Routes *RoutesConfig `json:"routes,omitempty"`

	// ClaudeResume tunes Claude Code's interactive "Resume from summary"
	// prompt for tclaude-spawned panes — see ClaudeResumeConfig. Absent /
	// nil keeps Claude Code's own defaults.
	ClaudeResume *ClaudeResumeConfig `json:"claude_resume,omitempty"`

	// ClaudeCleanupPeriodDays overrides Claude Code's own cleanupPeriodDays
	// retention setting — the number of days of inactivity after which Claude
	// Code deletes a conversation transcript (and other stale session data /
	// orphaned worktrees) at startup. Claude Code's built-in default is 30
	// days. When > 0, tclaude writes this value into the operator's
	// ~/.claude/settings.json on every session start, so tclaude-managed
	// transcripts survive far longer (set a large value like 99999 to
	// effectively keep them forever — Claude Code rejects 0 and has no
	// "never" sentinel). 0 / absent means tclaude leaves the key alone, so
	// Claude Code's default, or whatever the operator set by hand, stands.
	// Unlike the claude_resume overrides (env vars on tclaude panes only),
	// this IS persisted to ~/.claude/settings.json, so it also protects
	// transcripts from your own plain `claude` runs. See
	// ClaudeCleanupPeriodDaysOverride.
	ClaudeCleanupPeriodDays int `json:"claude_cleanup_period_days,omitempty"`

	// Processes holds operator policy for processes. Absent → all defaults.
	Processes *ProcessesConfig `json:"processes,omitempty"`

	// Taskboard is the group task board's retention policy — see
	// TaskboardConfig. The board itself is behind features.taskboard.
	Taskboard *TaskboardConfig `json:"taskboard,omitempty"`

	// Dashboard holds display toggles for the agentd web dashboard that
	// don't belong to the slop / cost / notification blocks — see
	// DashboardConfig. Absent → all defaults.
	Dashboard *DashboardConfig `json:"dashboard,omitempty"`

	// Session holds session-launch tuning — tmux-session naming and optional
	// model-assisted display naming. Absent → all defaults.
	Session *SessionConfig `json:"session,omitempty"`

	// TUI holds the color scheme for tclaude's interactive terminal views
	// (`session ls -w`, `conv ls -w`, `agent inbox -w`) — see TUIConfig.
	// Absent → the default scheme.
	TUI *TUIConfig `json:"tui,omitempty"`

	// Usage tunes the dashboard's subscription-usage readout (the top-bar
	// 5h/7d bars). Absent / blank falls back to the built-in defaults; see
	// ResolvedUsageIdleTimeout and PollAnthropicUsageAPI.
	Usage *UsageConfig `json:"usage,omitempty"`

	// Features holds switches for features under active development — see
	// FeaturesConfig. Each switch documents its own absent-value default.
	Features *FeaturesConfig `json:"features,omitempty"`
}

// FeaturesConfig holds switches for features under active development.
// Big features are built as dark increments on main behind these flags
// (rather than on long-lived feature branches), so regular users see nothing
// until a feature graduates and its flag is removed. Each flag documents its
// default; change it here or in the dashboard Config tab to test locally.
type FeaturesConfig struct {
	// GroupsRouteMap enables the opt-in read-only Members | Route map subview
	// in the Groups dashboard. It defaults off so the route projection and its
	// additional snapshot reads stay absent until an operator opts in.
	GroupsRouteMap bool `json:"groups_route_map,omitempty"`

	// GroupAttachments selects how the in-development persistent http(s)
	// attachment control appears on group titles. It defaults off while the
	// interaction design is being refined. Disabling it hides the dashboard
	// surface without deleting attachments already stored for groups.
	GroupAttachments GroupAttachmentsMode `json:"group_attachments,omitempty"`

	// TerminalCommandPaletteShortcut lets Ctrl/Cmd+K open the dashboard command
	// palette while focus is inside a web terminal. It defaults off because the
	// harnesses use that chord to clear the current input line.
	TerminalCommandPaletteShortcut bool `json:"terminal_command_palette_shortcut,omitempty"`

	// RecordedSandboxDetails shows the details chevron beside each agent's
	// sandbox badge. It defaults off because the badge tooltip already exposes
	// the common status and action; the chevron is an opt-in diagnostic surface.
	RecordedSandboxDetails bool `json:"recorded_sandbox_details,omitempty"`

	// AgentDirsMountParent switches how agent-owned directories are granted to
	// the sandbox. On (the default): the shared parent root
	// (agent-dirs/<launch-key>) is granted rw once, so the agent can create,
	// rewrite, and delete its own env-var'd directories. Set false to restore
	// per-directory grants: the agent can write inside each directory but cannot
	// delete the directory itself because its parent is not writable.
	AgentDirsMountParent *bool `json:"agent_dirs_mount_parent,omitempty"`

	// Taskboard enables the group task board: a short-term, ordered task list
	// per agent group (tclaude agent taskboard, /v1/groups/{g}/taskboard). It
	// defaults off while the board is built in slices; off, its routes answer
	// 404 and the sweep does nothing. Stored tasks are kept either way.
	Taskboard bool `json:"taskboard,omitempty"`
}

// GroupAttachmentsMode values — config features.group_attachments.
//
//	"off"   — hide the dashboard surface (also the absent/default mode).
//	"float" — show the hover-revealed paperclip overlay above the group title.
//	"fixed" — show an always-visible quick item at the right of the group
//	            header; only its edit pencil is hover-revealed.
type GroupAttachmentsMode string

const (
	GroupAttachmentsOff   GroupAttachmentsMode = "off"
	GroupAttachmentsFloat GroupAttachmentsMode = "float"
	GroupAttachmentsFixed GroupAttachmentsMode = "fixed"
)

// ProcessesConfig is operator policy for processes.
type ProcessesConfig struct {
	// RefuseUnsignedImports refuses to import a .pwf workflow file without a
	// valid signature. A review still shows such a file, and why it is
	// refused. Off by default: an unsigned file is valid but unverified.
	RefuseUnsignedImports bool `json:"refuse_unsigned_imports,omitempty"`
}

// TaskboardConfig is the group task board's retention policy. A done or
// dropped task leaves the live list at once; it is hidden from `ls --all`
// after ArchiveAfterDays and deleted, with its history, after RetentionDays.
type TaskboardConfig struct {
	// ArchiveAfterDays defaults to 14 when absent or not positive.
	ArchiveAfterDays int `json:"archive_after_days,omitempty"`
	// RetentionDays defaults to 90 when absent or not positive. A value below
	// the archive period is raised to it.
	RetentionDays int `json:"retention_days,omitempty"`
}

// Default task board retention, in days.
const (
	DefaultTaskboardArchiveAfterDays = 14
	DefaultTaskboardRetentionDays    = 90
)

// TaskboardEnabled reports whether features.taskboard is on. Nil-safe.
func (c *Config) TaskboardEnabled() bool {
	return c != nil && c.Features != nil && c.Features.Taskboard
}

// TaskboardRetention returns the archive and purge periods, in days.
func (c *Config) TaskboardRetention() (archiveDays, retentionDays int) {
	archiveDays, retentionDays = DefaultTaskboardArchiveAfterDays, DefaultTaskboardRetentionDays
	if c != nil && c.Taskboard != nil {
		if c.Taskboard.ArchiveAfterDays > 0 {
			archiveDays = c.Taskboard.ArchiveAfterDays
		}
		if c.Taskboard.RetentionDays > 0 {
			retentionDays = c.Taskboard.RetentionDays
		}
	}
	if retentionDays < archiveDays {
		retentionDays = archiveDays
	}
	return archiveDays, retentionDays
}

// RefuseUnsignedWorkflows reports whether importing an unsigned .pwf is refused.
func (c *Config) RefuseUnsignedWorkflows() bool {
	return c != nil && c.Processes != nil && c.Processes.RefuseUnsignedImports
}

// GroupsRouteMapEnabled reports whether the opt-in Groups Members | Route map
// dashboard subview is enabled. It defaults off and is nil-safe.
func (c *Config) GroupsRouteMapEnabled() bool {
	return c != nil && c.Features != nil && c.Features.GroupsRouteMap
}

// GroupAttachmentsMode reports how the dashboard should expose the
// experimental per-group persistent attachment control. It defaults off for a
// nil config, absent value, or unknown hand-edited value. Stored attachments
// are independent of this presentation setting.
func (c *Config) GroupAttachmentsMode() GroupAttachmentsMode {
	if c != nil && c.Features != nil {
		switch c.Features.GroupAttachments {
		case GroupAttachmentsFloat, GroupAttachmentsFixed:
			return c.Features.GroupAttachments
		}
	}
	return GroupAttachmentsOff
}

// TerminalCommandPaletteShortcutEnabled reports whether Ctrl/Cmd+K should be
// claimed by the dashboard while focus is inside a web terminal. It defaults
// off and is nil-safe so the chord continues to reach every harness unless the
// operator explicitly opts in.
func (c *Config) TerminalCommandPaletteShortcutEnabled() bool {
	return c != nil && c.Features != nil && c.Features.TerminalCommandPaletteShortcut
}

// RecordedSandboxDetailsEnabled reports whether the dashboard should show the
// recorded-launch details chevron beside sandbox badges. It defaults off and
// is nil-safe so the extra row chrome appears only after an explicit opt-in.
func (c *Config) RecordedSandboxDetailsEnabled() bool {
	return c != nil && c.Features != nil && c.Features.RecordedSandboxDetails
}

// AgentDirsMountParentEnabled reports whether agent-owned directories should be
// granted by mounting their shared parent root rw instead of granting each
// declared directory individually. It defaults on and is nil-safe.
func (c *Config) AgentDirsMountParentEnabled() bool {
	if c == nil || c.Features == nil || c.Features.AgentDirsMountParent == nil {
		return true
	}
	return *c.Features.AgentDirsMountParent
}

// StatusSnapshotConfig controls the shared runtime gathering window for every consumer.
type StatusSnapshotConfig struct {
	Disabled    bool `json:"disabled,omitempty"`
	FreshnessMS int  `json:"freshness_ms,omitempty"`
}
