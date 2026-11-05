// Package config provides configuration loading for tclaude.
package config

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/common"
)

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

	// Processes enables the in-development Processes feature — BPMN-lite
	// repeatable process graphs (drag-and-drop template editor, long-running
	// instantiated runs, live viewer). While in development the flag gates the
	// feature's user-visible surfaces (dashboard tab, CLI command, daemon
	// routes) as they land.
	Processes bool `json:"processes,omitempty"`

	// Triggers enables the in-development event-to-action automation engine.
	// It defaults off while trigger sources, actions, and dashboard controls
	// are landing in bounded slices.
	Triggers bool `json:"triggers,omitempty"`

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

// ProcessesDisabledMessage is the stable operator-facing text surfaced when
// the experimental Processes feature flag is off. The daemon is the sole
// authority on the flag (it reads private config; sandboxed agent clients
// never do), so both the daemon's process-route gate and the process CLI's
// daemon capability probe render this exact wording — enabled/disabled reads
// identically regardless of which layer detected it.
const ProcessesDisabledMessage = "process commands are disabled; set features.processes=true in tclaude config to use this experimental surface"

// TriggersDisabledMessage is the stable response returned by daemon trigger
// routes while the experimental feature is disabled.
const TriggersDisabledMessage = "trigger commands are disabled; set features.triggers=true in tclaude config to use this experimental surface"

// ProcessesConfig is operator policy for processes.
type ProcessesConfig struct {
	// RefuseUnsignedImports refuses to import a .pwf workflow file without a
	// valid signature. A review still shows such a file, and why it is
	// refused. Off by default: an unsigned file is valid but unverified.
	RefuseUnsignedImports bool `json:"refuse_unsigned_imports,omitempty"`
}

// RefuseUnsignedWorkflows reports whether importing an unsigned .pwf is refused.
func (c *Config) RefuseUnsignedWorkflows() bool {
	return c != nil && c.Processes != nil && c.Processes.RefuseUnsignedImports
}

// ProcessesEnabled reports whether the opt-in Processes feature flag is set.
// Nil-safe on both the config and the features block, so callers can gate on
// a bare Load() result without nil checks.
func (c *Config) ProcessesEnabled() bool {
	return c != nil && c.Features != nil && c.Features.Processes
}

// TriggersEnabled reports whether the opt-in Triggers feature flag is set.
// It is nil-safe and defaults off.
func (c *Config) TriggersEnabled() bool {
	return c != nil && c.Features != nil && c.Features.Triggers
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

// TUI color schemes — config tui.color_scheme. Picks the color palette the
// interactive bubbletea "watch" views (`session ls -w`, `conv ls -w`, `agent
// inbox -w`) render with:
//
//	"default"            — the current palette (PR #738), tuned to stay
//	                       readable on light AND dark terminals; a little
//	                       dimmer on a dark background.
//	"dark-high-contrast" — the brighter pre-#738 palette (vivid yellow / green
//	                       / red, brighter header), higher contrast on a dark
//	                       terminal at the cost of light-terminal readability.
//
// An empty / unknown value falls back to "default", so a typo can never leave
// the views unstyled. The palette values themselves live in
// pkg/claude/common/tuistyle; this is only the selector the config file and
// the dashboard Config tab edit.
const (
	TUIColorSchemeDefault      = "default"
	TUIColorSchemeHighContrast = "dark-high-contrast"
)

// TUIConfig holds the interactive-TUI color-scheme selection.
type TUIConfig struct {
	// ColorScheme names the palette — one of the TUIColorScheme* constants.
	// Empty / unknown resolves to TUIColorSchemeDefault (see TUIColorScheme).
	ColorScheme string `json:"color_scheme,omitempty"`
}

// normalizeTUIColorScheme returns s when it's a known scheme, else "" (so the
// resolver falls back to its default for a blank or hand-edited garbage value).
func normalizeTUIColorScheme(s string) string {
	switch s {
	case TUIColorSchemeDefault, TUIColorSchemeHighContrast:
		return s
	default:
		return ""
	}
}

// TUIColorScheme reports the effective interactive-TUI color scheme — config
// tui.color_scheme. Default "default" (absent block / key or an unknown
// value); "dark-high-contrast" selects the brighter pre-#738 palette. Nil-safe
// on the receiver so callers need no guard.
func (c *Config) TUIColorScheme() string {
	if c != nil && c.TUI != nil {
		if s := normalizeTUIColorScheme(c.TUI.ColorScheme); s != "" {
			return s
		}
	}
	return TUIColorSchemeDefault
}

// Tmux-session naming styles — config session.tmux_name_style. The style
// picks the BASE for a spawned session's tmux name when no explicit
// --label is given; session.UniqueTmuxSessionName still disambiguates a
// taken base with a -N suffix, and the DB row keeps the full identity
// either way — the tmux name is only the human-facing handle (JOH-248),
// so the style can be switched (or switched back) at any time and only
// affects newly launched sessions.
//
//	"id"  — first 8 chars of the session id (the historical default)
//	"dir" — sanitized basename of the session's working directory, for
//	        recognisable names when switching sessions inside tmux
//
// An empty / unknown value falls back to "id", so a typo can never change
// launch behavior.
const (
	TmuxNameStyleID  = "id"
	TmuxNameStyleDir = "dir"
)

// SessionConfig holds session-launch tuning.
type SessionConfig struct {
	// TmuxNameStyle picks the tmux-session naming style — one of the
	// TmuxNameStyle* constants above. Applies to `session new` without
	// --label and to conversation resumes; agentd-spawned agents always
	// pass their agent name as the label and are unaffected.
	TmuxNameStyle string `json:"tmux_name_style,omitempty"`

	// AutoNameFromPrompt lets agentd make a bounded, non-interactive model
	// call to replace a free-floating session's deterministic display-name
	// fallback after its first prompt. It defaults off: starting a session
	// must not silently spend tokens or wait on another model invocation.
	AutoNameFromPrompt bool `json:"auto_name_from_prompt,omitempty"`

	// AutoJoinGroup lets a fresh terminal-owned `tclaude` / `session new`
	// launch discover an active agent group whose default cwd is the same
	// canonical directory as the launch cwd, then use the ordinary daemon spawn
	// path for that group. It is a pointer because the out-of-box default is ON:
	// nil / absent means true, while an explicit false disables discovery.
	AutoJoinGroup *bool `json:"auto_join_group,omitempty"`

	// AutoJoinOrCreateGroup extends directory discovery by creating a group when
	// no active group owns the canonical launch cwd. The derived group name is
	// based on the directory basename and disambiguated predictably. This is
	// deliberately opt-in: nil / absent means false.
	AutoJoinOrCreateGroup *bool `json:"auto_join_or_create_group,omitempty"`
}

// ResolvedTmuxNameStyle returns the effective tmux-session naming style,
// normalized to one of the TmuxNameStyle* constants. Nil-safe; empty and
// unknown values resolve to TmuxNameStyleID (the historical id-prefix
// names).
func (c *Config) ResolvedTmuxNameStyle() string {
	if c == nil || c.Session == nil {
		return TmuxNameStyleID
	}
	if c.Session.TmuxNameStyle == TmuxNameStyleDir {
		return TmuxNameStyleDir
	}
	return TmuxNameStyleID
}

// AutoNameFromPromptEnabled reports whether free-floating sessions may use a
// one-shot model call to refine their deterministic display-name fallback.
// Nil-safe and opt-in so the default launch path remains free and immediate.
func (c *Config) AutoNameFromPromptEnabled() bool {
	return c != nil && c.Session != nil && c.Session.AutoNameFromPrompt
}

// AutoJoinGroupEnabled reports the terminal-start directory discovery policy.
// It defaults on so a group configured for the current directory works with a
// bare `tclaude`; only an explicit false disables it.
func (c *Config) AutoJoinGroupEnabled() bool {
	return c == nil || c.Session == nil || c.Session.AutoJoinGroup == nil || *c.Session.AutoJoinGroup
}

// AutoJoinOrCreateGroupEnabled reports whether terminal startup may create a
// missing directory group. It is opt-in and therefore defaults off.
func (c *Config) AutoJoinOrCreateGroupEnabled() bool {
	return c != nil && c.Session != nil && c.Session.AutoJoinOrCreateGroup != nil && *c.Session.AutoJoinOrCreateGroup
}

// Activity-bot style values — the per-mode choices in ActivityBotsConfig.
// "emoji" is the lightweight emoji/glyph+CSS bot row (fantasy glyphs in wizard
// mode); "sprites" is the pixel-art animation (robots in slop mode,
// spellcasters in wizard mode); "off" hides the indicator. An empty / unknown
// value falls back to the per-mode default (see ActivityBotsRegular /
// ActivityBotsSlop / ActivityBotsWizard).
const (
	ActivityBotsEmoji   = "emoji"
	ActivityBotsSprites = "sprites"
	ActivityBotsOff     = "off"
)

// Group quick-options display modes — config dashboard.group_quick_options.
// The "quick options" are the editable chips packed into each group's
// <summary> header (📝 description, 📁 default dir, 🧠 default profile, 🔗
// links). They grow the header wide, so the dashboard can auto-fold them:
//
//	"hover"    — icon-only at rest; the chip text slides open when the
//	             pointer is over the group header (a CSS horizontal
//	             accordion). The activity-bot row, group name and 👥 member
//	             chip always stay visible. This is the default.
//	"expanded" — always show the full chips (the pre-fold behaviour).
//
// An empty / unknown value falls back to the default (see GroupQuickOptions).
// Folding is a hover affordance, so it's gated to hover-capable pointers in
// CSS — touch devices always see the full chips. A per-group "pin" (a
// per-browser dashboard pref) opts a single group out of folding regardless
// of this mode.
const (
	GroupQuickOptionsHover    = "hover"
	GroupQuickOptionsExpanded = "expanded"
)

// Default-terminal modes — config dashboard.default_terminal. Chooses how the
// dashboard's per-agent "focus" / "open window" / "open terminal" actions open
// a console:
//
//	"native" — pop a native OS terminal window (the historical default),
//	           falling back to an in-browser PTY only when no native window
//	           can be opened.
//	"web"    — open the console as an in-browser terminal pane in the
//	           dashboard's own Terminals tab, without touching the OS windowing
//	           system — the same surface the dedicated "web term" / "web window"
//	           buttons already always use. This is the default.
//
// An empty / unknown value falls back to the default (see DefaultTerminal).
const (
	DefaultTerminalNative = "native"
	DefaultTerminalWeb    = "web"
)

// Default-directory-picker modes — config dashboard.default_directory_picker.
// Remote dashboard origins always override the local default to web mode.
const (
	DefaultDirectoryPickerNative = "native"
	DefaultDirectoryPickerWeb    = "web"
)

// DashboardConfig holds display toggles for the agentd web dashboard.
type DashboardConfig struct {
	// TerminalAttach tunes the browser terminal's resize handshake when it
	// attaches to a tmux client. Absent keeps the historical repair sequence:
	// send the fitted size, then nudge the terminal by one row and restore it
	// after the first screen arrives. See TerminalAttachConfig and
	// (*Config).ResolvedTerminalAttach.
	TerminalAttach *TerminalAttachConfig `json:"terminal_attach,omitempty"`
	// CloseTerminalTabs decides which dashboard terminal tabs close on their
	// own when their tmux side goes away, per event and per kind of terminal.
	// Absent keeps the defaults: shell terminals close when their session
	// ends, agent windows and every detach keep the tab. See
	// TerminalTabCloseConfig and (*Config).CloseTerminalTab.
	CloseTerminalTabs *TerminalTabCloseConfig `json:"close_terminal_tabs,omitempty"`
	// Keybindings rebinds dashboard keyboard shortcuts: shortcut id (from the
	// dashboard's registry, js/keybindings.js) to a chord such as
	// "Ctrl+Alt+KeyK". Absent ids keep their default. The dashboard owns the
	// meaning; the daemon only checks the shape. See (*Config).DashboardKeybindings.
	Keybindings map[string]string `json:"keybindings,omitempty"`
	// ActivityBots selects the style of the per-group + global "activity
	// bot" indicator — the deduped row of robot icons that rides in each
	// group <summary> header (visible even when the group is folded) and in
	// the top bar, summarising member status at a glance. The style is
	// chosen INDEPENDENTLY for the plain dashboard and for slop mode — see
	// ActivityBotsConfig. Absent → defaults (emoji in regular, sprites in
	// slop).
	ActivityBots *ActivityBotsConfig `json:"activity_bots,omitempty"`
	// AlwaysShowPluginsTab forces the dashboard's Plugins tab to stay
	// visible even when no plugins are installed. By default the tab
	// auto-hides when the installed set is empty (most users never define a
	// plugin, and the tab would only show the built-in catalog) — flip this
	// on to keep it around, e.g. to reach the catalog and install one. A
	// broken plugins.json still surfaces the tab regardless, so the error is
	// never hidden. Default false (auto-hide when empty). See
	// (*Config).ShowPluginsTabAlways.
	AlwaysShowPluginsTab bool `json:"always_show_plugins_tab,omitempty"`
	// HScrollFollow selects the dashboard's horizontal-scroll chrome-bar
	// behaviour for when the page is wide enough to need a sideways scrollbar
	// (JOH-313). The full-bleed bars (header / nav / slop marquee) always
	// widen to the content so they never look ragged; this knob is only about
	// their CONTENT:
	//   true  (follow, the default) — the bars' content is pinned to the
	//         viewport and sticky-left, so the header controls + tab strip
	//         stay put and usable while the page is scrolled sideways.
	//   false (static)              — the content scrolls off with the page;
	//         the bar background still fills the width, but the controls
	//         aren't reachable while scrolled right.
	// A *bool so absent (the default) is distinguishable from an explicit
	// false: nil → follow. The dashboard reads the resolved value off the
	// snapshot each poll and toggles body.hscroll-follow; it replaces the old
	// per-browser header toggle button. See (*Config).HScrollFollow.
	HScrollFollow *bool `json:"hscroll_follow,omitempty"`
	// AlwaysShowOpenPRs keeps the fixed footer's "Open PRs" indicator
	// mounted even when the active GitHub identity has no open pull requests
	// at all. Default true: the indicator is a permanent, glanceable entry
	// point to the popover (which also carries the recently closed/merged
	// list), so it should not appear and disappear as the last PR merges.
	// An explicit false restores the older behaviour of showing it only
	// while at least one authored PR is open. Either way the indicator stays
	// hidden until the daemon has actually resolved a GitHub identity — a
	// permanent "Open PRs 0" would be a lie when `gh` is missing or logged
	// out. A *bool so absent (the default) is distinguishable from an
	// explicit false. See (*Config).AlwaysShowOpenPRs.
	AlwaysShowOpenPRs *bool `json:"always_show_open_prs,omitempty"`
	// RecentPRWindowDays bounds the footer popover's "Recently closed"
	// filter: authored pull requests merged or closed within this many days
	// back. Default 3 (absent / nil). 0 disables the filter entirely and the
	// daemon then stops searching for closed PRs at all; larger values are
	// clamped to RecentPRWindowDaysMax so a hand-edited config cannot ask
	// GitHub for an unbounded history every poll. A *int so an explicit 0
	// (off) is distinguishable from an absent key (default). See
	// (*Config).RecentPRWindowDays.
	RecentPRWindowDays *int `json:"recent_pr_window_days,omitempty"`
	// DictationSendPhrase is the send phrase's earlier home, still read when
	// Voice lists none. A *string so an explicit "" (off) differs from absent.
	DictationSendPhrase *string `json:"dictation_send_phrase,omitempty"`
	// Voice holds the spoken phrases and the opt-in hands-free settings. See
	// (*Config).Voice.
	Voice *VoiceConfig `json:"voice,omitempty"`
	// GroupQuickOptions selects how the editable "quick option" chips in each
	// group <summary> header (📝 description, 📁 default dir, 🧠 default
	// profile, 🔗 links) are displayed — one of GroupQuickOptions{Hover,
	// Expanded}. "hover" (the default) folds them to icon-only at rest and
	// slides the text open on header hover, reclaiming horizontal space;
	// "expanded" keeps the full chips always visible. Empty / unknown →
	// default (hover). The dashboard reads the resolved value off the snapshot
	// each poll and toggles body.group-quick-fold. See
	// (*Config).GroupQuickOptions.
	GroupQuickOptions string `json:"group_quick_options,omitempty"`
	// OverlayBackdrop picks what full-screen previews do to the page behind
	// them: "dim" (default), "blur", "both" or "none". Blur is slow in a
	// browser without GPU acceleration. See (*Config).OverlayBackdrop.
	OverlayBackdrop string `json:"overlay_backdrop,omitempty"`
	// WindowControls picks the look of an overlay window's title bar buttons:
	// "auto" (default) or "standard" for plain − ⇄ × symbols, or the opt-in
	// native looks: "os" (follows the browser's OS), "macos", "windows",
	// "linux", or "classic". See (*Config).WindowControls.
	WindowControls string `json:"window_controls,omitempty"`
	// NoSnapAssist turns off Snap Assist: after the operator snaps an overlay
	// window, the space left empty no longer offers the other windows. See
	// (*Config).SnapAssist.
	NoSnapAssist bool `json:"no_snap_assist,omitempty"`
	// DefaultTerminal selects how the dashboard's spawn auto-focus, per-agent
	// focus / open-window / open-terminal actions open a console — one of
	// DefaultTerminal{Native, Web}. "native" pops a native OS window (falling
	// back to an in-browser PTY only when it can't); "web" (the default) opens
	// an in-browser terminal pane in the dashboard's Terminals tab instead, the
	// same surface the dedicated "web term" / "web window" buttons use. Empty /
	// unknown → default (web). The dashboard reads the resolved value off the
	// snapshot and routes its focus/open actions accordingly.
	// See (*Config).DefaultTerminal.
	DefaultTerminal string `json:"default_terminal,omitempty"`
	// DefaultDirectoryPicker selects the directory chooser used from a local
	// dashboard: "native" opens the host OS dialog, while "web" (the default)
	// uses the dashboard's browser-rendered directory navigator. Remote
	// dashboard origins always use the web picker because a host-side native
	// dialog cannot be operated from the remote browser. Empty / unknown falls
	// back to web. See (*Config).DefaultDirectoryPicker.
	DefaultDirectoryPicker string `json:"default_directory_picker,omitempty"`
	// ShowAgentHideButton keeps the per-agent "hide window" button — the
	// slashed-eye icon beside "focus" in each agent row's quick-control
	// cluster — visible. That button detaches the agent's terminal window;
	// it's far less used than its "focus" (show-window) twin, so by default
	// the dashboard drops it to keep the row tight, leaving just "focus". Flip
	// this on to bring the hide button back. Default false (hidden). The
	// dashboard reads the resolved value off the snapshot each poll and
	// toggles body.show-agent-hide-btn. See (*Config).ShowAgentHideButton.
	ShowAgentHideButton bool `json:"show_agent_hide_button,omitempty"`
	// ShowGroupDescription keeps each group header's 📝 description chip — the
	// click-to-edit blurb beside the group name — visible. The feature is
	// deprecated: group descriptions are display-only, never surfaced anywhere
	// that drives behaviour, so the chip is hidden by default to keep the
	// header uncluttered. Flip this on to bring the chip back (and with it the
	// only way to view/edit a group's description). Default false (hidden). The
	// dashboard reads the resolved value off the snapshot each poll and toggles
	// body.show-group-description. See (*Config).ShowGroupDescription.
	ShowGroupDescription bool `json:"show_group_description,omitempty"`
	// ShowFinalPRChecks keeps the CI checks pill (✓ n/m) beside merged and
	// closed pull requests in the Groups member table. Once a PR is merged or
	// closed its CI result is history, so by default the dashboard only keeps
	// the pill when the final run is failing; flip this on to always show it.
	// Default false. The dashboard reads the resolved value off the snapshot
	// each poll and toggles body.show-final-pr-checks. See
	// (*Config).ShowFinalPRChecks.
	ShowFinalPRChecks bool `json:"show_final_pr_checks,omitempty"`
	// StayOnTerminalsAfterLastClose keeps the dashboard on the Terminals tab
	// when its last open terminal closes. By default the dashboard moves to
	// the Groups tab instead, since an empty Terminals tab has nothing to act
	// on. Flip this on to keep the old stay-put behavior. Default false. See
	// (*Config).StayOnTerminalsAfterLastClose.
	StayOnTerminalsAfterLastClose bool `json:"stay_on_terminals_after_last_close,omitempty"`
	// FusedToolbarFixedNode keeps the fused Groups view's toolbar (global
	// defaults, + new group) on the node picked in its node chip. By default
	// it follows the node of the group last acted on there. Default false.
	// See (*Config).FusedToolbarFixedNode.
	FusedToolbarFixedNode bool `json:"fused_toolbar_fixed_node,omitempty"`
	// UsageCollapseSubviews restores the Usage tab's old auto-collapse: with
	// only one of Quota/Costs having data, the tab shows that view alone and
	// hides the Overview/Quota/Costs switcher. By default all three views are
	// always offered. Default false. See (*Config).UsageCollapseSubviews.
	UsageCollapseSubviews bool `json:"usage_collapse_subviews,omitempty"`
}

// Browser-terminal attach resize modes — config dashboard.terminal_attach.mode.
const (
	TerminalAttachResizeRepair    = "repair"
	TerminalAttachResizeInitial   = "initial"
	TerminalAttachResizePreAttach = "pre_attach"

	DefaultTerminalAttachRepairDelayMS    = 250
	DefaultTerminalAttachPreAttachDelayMS = 250
	MaxTerminalAttachDelayMS              = 10000
)

// TerminalAttachConfig controls how an xterm-backed terminal establishes and
// repairs its initial geometry. All delays are milliseconds and pointers so an
// explicit zero remains distinct from an absent value.
//
// Modes:
//   - repair (default): initial resize, then the historical one-row nudge and
//     restore after RepairDelayMS.
//   - initial: initial resize only.
//   - pre_attach: send the initial size before the PTY command starts, wait
//     PreAttachDelayMS, then start the attachment; no post-attach nudge.
type TerminalAttachConfig struct {
	Mode                 string `json:"mode,omitempty"`
	InitialResizeDelayMS *int   `json:"initial_resize_delay_ms,omitempty"`
	RepairDelayMS        *int   `json:"repair_delay_ms,omitempty"`
	PreAttachDelayMS     *int   `json:"pre_attach_delay_ms,omitempty"`
}

// ResolvedTerminalAttach returns the effective browser-terminal attach mode
// and delays. Unknown modes degrade to the historical repair behavior while
// Validate reports the bad value to the Config tab. Invalid hand-edited delays
// are clamped so the live dashboard remains usable while validation reports
// them.
func (c *Config) ResolvedTerminalAttach() (mode string, initialDelayMS, repairDelayMS, preAttachDelayMS int) {
	mode = TerminalAttachResizeRepair
	repairDelayMS = DefaultTerminalAttachRepairDelayMS
	preAttachDelayMS = DefaultTerminalAttachPreAttachDelayMS
	if c == nil || c.Dashboard == nil || c.Dashboard.TerminalAttach == nil {
		return
	}
	t := c.Dashboard.TerminalAttach
	switch t.Mode {
	case "", TerminalAttachResizeRepair:
	case TerminalAttachResizeInitial, TerminalAttachResizePreAttach:
		mode = t.Mode
	}
	resolve := func(value *int, fallback int) int {
		if value == nil {
			return fallback
		}
		return min(MaxTerminalAttachDelayMS, max(0, *value))
	}
	initialDelayMS = resolve(t.InitialResizeDelayMS, 0)
	repairDelayMS = resolve(t.RepairDelayMS, DefaultTerminalAttachRepairDelayMS)
	preAttachDelayMS = resolve(t.PreAttachDelayMS, DefaultTerminalAttachPreAttachDelayMS)
	return
}

// TerminalTabEvent names why a dashboard terminal's tmux client went away —
// the rows of dashboard.close_terminal_tabs.
type TerminalTabEvent string

const (
	// TerminalTabSessionEnded: the tmux session is gone (the shell exited, the
	// agent exited, or the session was killed).
	TerminalTabSessionEnded TerminalTabEvent = "session_ended"
	// TerminalTabDetached: the tmux client exited but its session lives on
	// (prefix+d, or another client took the session over).
	TerminalTabDetached TerminalTabEvent = "detached"
)

// TerminalTabTarget names the kind of dashboard terminal — the columns of
// dashboard.close_terminal_tabs.
type TerminalTabTarget string

const (
	// TerminalTabGroupTerminal is a shell opened in a group's home dir.
	TerminalTabGroupTerminal TerminalTabTarget = "group_terminal"
	// TerminalTabAgentTerminal is a shell opened in an agent's directory.
	TerminalTabAgentTerminal TerminalTabTarget = "agent_terminal"
	// TerminalTabAgentWindow is an agent's own harness pane.
	TerminalTabAgentWindow TerminalTabTarget = "agent_window"
)

// TerminalTabCloseConfig is dashboard.close_terminal_tabs: one target set per
// event. Each switch is a *bool so absent keeps its default (see
// CloseTerminalTab) while an explicit false opts out.
type TerminalTabCloseConfig struct {
	SessionEnded *TerminalTabCloseTargets `json:"session_ended,omitempty"`
	Detached     *TerminalTabCloseTargets `json:"detached,omitempty"`
}

// TerminalTabCloseTargets holds one event's per-terminal-kind switches.
type TerminalTabCloseTargets struct {
	GroupTerminal *bool `json:"group_terminal,omitempty"`
	AgentTerminal *bool `json:"agent_terminal,omitempty"`
	AgentWindow   *bool `json:"agent_window,omitempty"`
}

// CloseTerminalTab reports whether a dashboard terminal tab of kind target
// should close itself on event. Defaults: shell terminals (group and agent
// dir) close once their session has ended, since reconnecting would only
// start a fresh shell; agent windows keep the tab so the exit stays visible,
// and a detach never closes anything. Nil-safe; unknown values are false.
func (c *Config) CloseTerminalTab(event TerminalTabEvent, target TerminalTabTarget) bool {
	def := event == TerminalTabSessionEnded &&
		(target == TerminalTabGroupTerminal || target == TerminalTabAgentTerminal)
	if c == nil || c.Dashboard == nil || c.Dashboard.CloseTerminalTabs == nil {
		return def
	}
	var targets *TerminalTabCloseTargets
	switch event {
	case TerminalTabSessionEnded:
		targets = c.Dashboard.CloseTerminalTabs.SessionEnded
	case TerminalTabDetached:
		targets = c.Dashboard.CloseTerminalTabs.Detached
	}
	if targets == nil {
		return def
	}
	var v *bool
	switch target {
	case TerminalTabGroupTerminal:
		v = targets.GroupTerminal
	case TerminalTabAgentTerminal:
		v = targets.AgentTerminal
	case TerminalTabAgentWindow:
		v = targets.AgentWindow
	}
	if v == nil {
		return def
	}
	return *v
}

// ActivityBotsConfig picks the activity-bot visual independently per mode,
// so the lightweight emoji bots can ride the plain dashboard while the full
// pixel-art robots come out for slop ("casino") mode — the defaults — or
// any other mix, or off entirely. Wizard mode defaults to its own fantasy
// glyphs and can opt into the wizard sprite sheets. Each field is one of
// ActivityBots{Emoji,Sprites,Off}; empty / unknown falls back to that
// mode's default. prefers-reduced-motion already drops just the animation
// (keeping the bots); these are the change-the-style / turn-it-off knobs.
type ActivityBotsConfig struct {
	Regular string `json:"regular,omitempty"` // plain dashboard; default emoji
	Slop    string `json:"slop,omitempty"`    // slop mode; default sprites (robots)
	Wizard  string `json:"wizard,omitempty"`  // wizard mode; default emoji (glyphs); "sprites" = wizards
}

// ClaudeResumeConfig tunes Claude Code's interactive "Resume from summary"
// chooser — the multiple-choice prompt CC shows when resuming a conversation
// that is BOTH old (≥ ThresholdMinutes since last activity) AND large
// (≥ TokenThreshold estimated tokens). That prompt breaks tclaude's scripted
// resume: a detached, tmux-driven pane can't answer a TUI it didn't expect, so
// the resume hangs. Raising either threshold high enough makes the prompt never
// fire (CC's gate shows it only when both conditions hold, so lifting one is
// enough) — `tclaude setup --install-resume-threshold-override` writes a large
// ThresholdMinutes for exactly this reason.
//
// tclaude applies these as the CLAUDE_CODE_RESUME_THRESHOLD_MINUTES /
// CLAUDE_CODE_RESUME_TOKEN_THRESHOLD environment variables on the `claude`
// process it spawns ONLY — it never writes them into ~/.claude/settings.json.
// That keeps the operator's manual `claude` runs untouched and makes this block
// (in ~/.tclaude/config.json) the single source of truth the dashboard Config
// tab and its diff viewer edit. The env vars are Claude-Code-specific, so the
// overrides are injected only when the spawned harness is Claude Code; Codex has
// no equivalent prompt and ignores the block.
//
// Both fields are pointers so "absent" is distinguishable from an explicit 0:
// a nil pointer omits the matching env var (Claude Code keeps its own built-in
// default — 70 minutes / 100,000 tokens), a set value injects it. The env vars
// are undocumented and version-specific (verified against Claude Code 2.1.187);
// if a future CC build renames or drops them the override degrades to a no-op,
// never an error — clear the block to revert.
type ClaudeResumeConfig struct {
	// ThresholdMinutes overrides CLAUDE_CODE_RESUME_THRESHOLD_MINUTES — the
	// minimum age (minutes since last activity) a conversation must reach
	// before the resume prompt is even considered. nil omits the var (CC's
	// 70-minute default). Set it very high (ResumeThresholdMinutesSuppress)
	// to suppress the prompt for tclaude's automation.
	ThresholdMinutes *int `json:"threshold_minutes,omitempty"`
	// TokenThreshold overrides CLAUDE_CODE_RESUME_TOKEN_THRESHOLD — the
	// minimum estimated context size (tokens) a conversation must reach
	// before the resume prompt is considered. nil omits the var (CC's
	// 100,000-token default). A secondary knob: raising ThresholdMinutes
	// alone already suppresses the prompt.
	TokenThreshold *int `json:"token_threshold,omitempty"`
}

// Claude Code resume-prompt environment variables. These gate CC's
// "Resume from summary" chooser; tclaude injects them per-spawn to keep the
// chooser from blocking a detached resume. Undocumented + version-specific
// (Claude Code 2.1.187) — treated as best-effort, so an unknown-to-CC name is
// simply ignored by the harness rather than an error here.
const (
	EnvResumeThresholdMinutes = "CLAUDE_CODE_RESUME_THRESHOLD_MINUTES"
	EnvResumeTokenThreshold   = "CLAUDE_CODE_RESUME_TOKEN_THRESHOLD"
)

// ResumeThresholdMinutesSuppress is the ThresholdMinutes value
// `tclaude setup --install-resume-threshold-override` writes to switch the
// "Resume from summary" prompt off for tclaude-spawned panes: 525,600,000
// minutes (1,000 years), so a resumed session's age can never reach it and the
// prompt never fires. A deliberately absurd, clearly-intentional sentinel —
// not a real threshold anyone would pick by hand.
const ResumeThresholdMinutesSuppress = 525_600_000

// ClaudeResumeEnv returns the CLAUDE_CODE_RESUME_* environment overrides to
// inject into a spawned Claude Code process, or an empty map when nothing is
// configured. Each set field contributes its env var; a nil field is omitted so
// Claude Code keeps its own default for that threshold. Nil-safe on the receiver
// so callers need no guard.
//
// It is harness-agnostic by construction — it just resolves the configured
// integers to their CC env-var names. The caller decides WHEN to apply them
// (only for the Claude Code harness), so this method never gates on harness.
func (c *Config) ClaudeResumeEnv() map[string]string {
	if c == nil || c.ClaudeResume == nil {
		return nil
	}
	env := map[string]string{}
	if c.ClaudeResume.ThresholdMinutes != nil {
		env[EnvResumeThresholdMinutes] = strconv.Itoa(*c.ClaudeResume.ThresholdMinutes)
	}
	if c.ClaudeResume.TokenThreshold != nil {
		env[EnvResumeTokenThreshold] = strconv.Itoa(*c.ClaudeResume.TokenThreshold)
	}
	return env
}

// ClaudeCleanupPeriodDaysOverride returns the tclaude-configured Claude Code
// transcript-retention override (config claude_cleanup_period_days) and whether
// tclaude should manage Claude Code's cleanupPeriodDays key at all. ok is false
// when the override is unset / non-positive, in which case Claude Code's own
// default (30 days) or the operator's hand-set settings.json value stands. When
// ok, days is the value tclaude writes into ~/.claude/settings.json. Nil-safe on
// the receiver.
func (c *Config) ClaudeCleanupPeriodDaysOverride() (days int, ok bool) {
	if c == nil || c.ClaudeCleanupPeriodDays <= 0 {
		return 0, false
	}
	return c.ClaudeCleanupPeriodDays, true
}

// AuditConfig configures the agentd audit log — the persistent trail of
// daemon-proxied tclaude commands surfaced on the dashboard's Audit tab.
type AuditConfig struct {
	// RetentionDays is how many days of audit rows to keep; the daemon's
	// periodic cleanup prunes anything older. 0 / absent means the
	// built-in default (DefaultAuditRetentionDays). A negative value
	// disables pruning (keep forever) — see ResolvedAuditRetentionDays.
	RetentionDays int `json:"retention_days,omitempty"`
}

// DefaultAuditRetentionDays is the out-of-box audit-log retention window
// when config.json pins none — 30 days of command history is a useful
// trail without letting the table grow without bound.
const DefaultAuditRetentionDays = 30

// ResolvedAuditRetentionDays returns the effective audit-log retention in
// days, and whether pruning is enabled. A configured negative value means
// "keep forever" (enabled=false); 0 / absent means the built-in default.
// Nil-safe so callers need no guard.
func (c *Config) ResolvedAuditRetentionDays() (days int, prune bool) {
	if c == nil || c.Audit == nil || c.Audit.RetentionDays == 0 {
		return DefaultAuditRetentionDays, true
	}
	if c.Audit.RetentionDays < 0 {
		return 0, false // keep forever
	}
	return c.Audit.RetentionDays, true
}

// FederationConfig configures the outbound hub connection.
// FederationAwayConfig is the operator's selected covering instance. Live
// approval authority is deliberately not persisted across daemon restarts.
type FederationAwayConfig struct {
	Cover string    `json:"cover"`
	Since time.Time `json:"since"`
	Until time.Time `json:"until,omitempty"`
}

// FederationHealthPolicy controls operator notices; resource/failure signals are opt-in.
type FederationHealthPolicy struct {
	Presence             *bool   `json:"presence,omitempty"`
	Resources            bool    `json:"resources"`
	Failures             bool    `json:"failures"`
	DebounceSeconds      int     `json:"debounce_seconds,omitempty"`
	DiskFreePercent      float64 `json:"disk_free_percent,omitempty"`
	RAMFreePercent       float64 `json:"ram_free_percent,omitempty"`
	MemorySeconds        int     `json:"memory_seconds,omitempty"`
	FailureCount         int     `json:"failure_count,omitempty"`
	FailureWindowSeconds int     `json:"failure_window_seconds,omitempty"`
	CooldownSeconds      int     `json:"cooldown_seconds,omitempty"`
}

func (p FederationHealthPolicy) Validate() error {
	for _, n := range []int{p.DebounceSeconds, p.MemorySeconds, p.FailureWindowSeconds, p.CooldownSeconds} {
		if n < 0 || n > 86400 {
			return fmt.Errorf("durations must be 0..86400 seconds (zero uses default)")
		}
	}
	if p.FailureCount < 0 || p.FailureCount > 256 {
		return fmt.Errorf("failure_count must be 0..256")
	}
	for _, n := range []float64{p.DiskFreePercent, p.RAMFreePercent} {
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 100 {
			return fmt.Errorf("free percentages must be 0..100")
		}
	}
	return nil
}

type FederationHealthConfig struct {
	Defaults FederationHealthPolicy `json:"defaults,omitempty"`
	// Keys are immutable instance IDs. Accepted linked successors inherit an
	// ancestor's settings unless they have an explicit override.
	Peers map[string]FederationHealthPolicy `json:"peers,omitempty"`
}

type NodeScriptsConfig struct {
	AcceptRemoteScripts bool     `json:"accept_remote_scripts"`
	Memory              string   `json:"memory,omitempty"`
	CPU                 *float64 `json:"cpu,omitempty"`
	PIDs                *uint64  `json:"pids,omitempty"`
}

// UpdatesConfig is how this node keeps itself up to date beyond tclaude's own
// binaries: Scripts are operator-written status/update commands shown in the
// fleet's Harnesses page, and Harnesses replaces a harness's default update
// command (its built-in updater) with the operator's own.
type UpdatesConfig struct {
	Scripts   []UpdateScriptConfig           `json:"scripts,omitempty"`
	Harnesses map[string]HarnessUpdateConfig `json:"harnesses,omitempty"`
}

// UpdateScriptConfig is one named update target. Status and Update are run
// with /bin/sh -c as the agentd user from the home directory; at least one is
// required. Status prints JSON ({"summary","update_available","items":[...]})
// or plain text, where exit 0 means up to date and exit 100 means updates are
// pending.
type UpdateScriptConfig struct {
	Name                 string `json:"name"`
	Label                string `json:"label,omitempty"`
	Status               string `json:"status,omitempty"`
	Update               string `json:"update,omitempty"`
	StatusTimeoutSeconds int    `json:"status_timeout_seconds,omitempty"`
	UpdateTimeoutSeconds int    `json:"update_timeout_seconds,omitempty"`
}

// HarnessUpdateConfig overrides one harness's update command.
type HarnessUpdateConfig struct {
	Update string `json:"update,omitempty"`
}

var updateScriptName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

// Validate reports problems with the update scripts and overrides.
func (u *UpdatesConfig) Validate() []string {
	if u == nil {
		return nil
	}
	var errs []string
	seen := map[string]bool{}
	for _, s := range u.Scripts {
		switch {
		case !updateScriptName.MatchString(s.Name):
			errs = append(errs, "updates.scripts: name must be 1-40 lowercase letters, digits, - or _, starting with a letter or digit")
		case seen[s.Name]:
			errs = append(errs, "updates.scripts: duplicate name "+s.Name)
		case strings.TrimSpace(s.Status) == "" && strings.TrimSpace(s.Update) == "":
			errs = append(errs, "updates.scripts."+s.Name+": set status, update or both")
		case s.StatusTimeoutSeconds < 0 || s.StatusTimeoutSeconds > 3600 || s.UpdateTimeoutSeconds < 0 || s.UpdateTimeoutSeconds > 86400:
			errs = append(errs, "updates.scripts."+s.Name+": status_timeout_seconds must be 0..3600 and update_timeout_seconds 0..86400")
		}
		seen[s.Name] = true
	}
	for name, h := range u.Harnesses {
		if strings.TrimSpace(h.Update) == "" {
			errs = append(errs, "updates.harnesses."+name+".update must not be empty")
		}
	}
	return errs
}

// SnapshotsConfig configures the local agent snapshot library.
type SnapshotsConfig struct {
	// DefaultRetention is how long a snapshot is kept when created without
	// an explicit expiry, as a duration such as "720h" or "30d". Empty
	// keeps snapshots until removed.
	DefaultRetention string `json:"default_retention,omitempty"`
}

type FederationConfig struct {
	// BoardUpdateNotify leaves a Messages note when a kept content-board item
	// gets a newer version. Off by default; the dashboard badge works regardless.
	BoardUpdateNotify bool `json:"board_update_notify,omitempty"`
	// AgentTransferMaxBytes caps agent archives and uncompressed history; zero defaults to 2 GiB.
	AgentTransferMaxBytes int64 `json:"agent_transfer_max_bytes,omitempty"`
	// AgentHistoryRecordMaxBytes is the JSONL record memory bound; zero defaults to256 MiB.
	AgentHistoryRecordMaxBytes int                     `json:"agent_history_record_max_bytes,omitempty"`
	Scripts                    *NodeScriptsConfig      `json:"scripts,omitempty"`
	Health                     *FederationHealthConfig `json:"health,omitempty"`

	// IdentityRotationSeconds is the local successor detection window; default 600.
	IdentityRotationSeconds int                       `json:"identity_rotation_seconds,omitempty"`
	Teleport                *FederationTeleportConfig `json:"teleport,omitempty"`
	// NodeLabels is a local set, shared by operator and future node-profile writers.
	NodeLabels []string `json:"node_labels,omitempty"`
	// MaxLiveAgents advertises node capacity and the receiver enforces it on
	// incoming work; zero is unlimited.
	MaxLiveAgents int                   `json:"max_live_agents,omitempty"`
	Away          *FederationAwayConfig `json:"away,omitempty"`
	// UnrestrictedMaxLive caps automatic workers per unrestricted peer. Default 8.
	UnrestrictedMaxLive int `json:"unrestricted_max_live,omitempty"`
	// Enabled starts the hub client. Default false.
	Enabled bool `json:"enabled,omitempty"`
	// HubURL is the hub's WebSocket URL: wss://host[:port], or ws:// to a
	// loopback hub only.
	HubURL string `json:"hub_url,omitempty"`
	// Name is this instance's display name on the hub (default
	// user@hostname).
	Name string `json:"name,omitempty"`
	// Invite is a single-use hub invite token, presented until admitted.
	Invite string `json:"invite,omitempty"`
	// HubCAFile is an optional PEM bundle trusted for the hub's TLS
	// certificate instead of the system roots.
	HubCAFile string `json:"hub_ca_file,omitempty"`
}

// RoutesConfig tunes the group-route data plane (docs/group-routes.md).
type RoutesConfig struct {
	// FlowControl enables per-stream credit flow control on route streams
	// whose endpoints both support it, so a slow reader slows its sender
	// instead of overflowing a buffer and resetting the connection. Absent
	// = true; false keeps the fixed bounded buffers of older releases.
	FlowControl *bool `json:"flow_control,omitempty"`
	// WindowKiB is the receive window, per stream and direction, that
	// agentd's endpoints and its route helpers use: how far a sender may
	// run ahead of its reader. Larger windows help bulk transfers over
	// high-latency paths (a federation hub) at the cost of memory per
	// stream. Default 256; clamped to 256..16384.
	WindowKiB int `json:"window_kib,omitempty"`
}

// RouteFlowWindow returns the route flow-control receive window in bytes,
// or 0 when flow control is disabled. Nil-safe.
func (c *Config) RouteFlowWindow() int {
	if c != nil && c.Routes != nil {
		if c.Routes.FlowControl != nil && !*c.Routes.FlowControl {
			return 0
		}
		if c.Routes.WindowKiB > 0 {
			return min(max(c.Routes.WindowKiB, 256), 16384) << 10
		}
	}
	return 256 << 10
}

// RemoteAccessConfig configures the optional, separately-bound HTTPS listener
// that exposes the agentd dashboard to the network (LAN / mesh / tunnel). It
// is OFF by default and entirely independent of the loopback dashboard, which
// keeps its init-token → session-cookie flow unchanged.
//
// When enabled, agentd starts a SECOND listener on Bind that enforces, before
// any dashboard/API request is served:
//   - mTLS — a client certificate issued by the tclaude remote-access CA
//     (RequireAndVerifyClientCert at the TLS layer; no valid cert ⇒ the
//     connection is refused before any handler runs), AND
//   - a passphrase login (`/login`) that mints a signed, restart-surviving
//     session cookie.
//
// This is a network-exposed agent control plane (it can spawn/kill agents and
// is a send-keys injection sink), so the auth is deliberately built to the
// public-internet bar; LAN is just the zero-infra preset of that hardened
// build. All secret material — the CA/server/client certs, the passphrase
// hash, and the cookie-signing key — lives as 0600 files under
// RemoteAccessDir (~/.tclaude/remote-access/), never in this config file.
// Generate it with `tclaude remote-access setup`.
type RemoteAccessConfig struct {
	// Enabled starts the remote HTTPS listener. Default false: tclaude never
	// exposes the control plane to the network without an explicit opt-in.
	Enabled bool `json:"enabled,omitempty"`

	// Bind is the listen address for the remote listener — e.g.
	// "0.0.0.0:8443" (LAN), a tailnet interface IP (mesh), or
	// "127.0.0.1:8443" (behind a tunnel that terminates a real cert). Empty
	// leaves the listener off even when Enabled is true (there is nothing to
	// bind to), and Validate flags that combination.
	Bind string `json:"bind,omitempty"`
}

// RemoteAccessEnabled reports whether the remote listener should start: the
// block is present, Enabled, and has a non-empty Bind. Nil-safe so callers
// need no guard.
func (c *Config) RemoteAccessEnabled() bool {
	return c != nil && c.RemoteAccess != nil && c.RemoteAccess.Enabled && c.RemoteAccess.Bind != ""
}

// RemoteAccessBind returns the configured remote listen address, or "" when no
// remote-access block is set. Nil-safe.
func (c *Config) RemoteAccessBind() string {
	if c == nil || c.RemoteAccess == nil {
		return ""
	}
	return c.RemoteAccess.Bind
}

// AskConfig is the persistent default profile for `tclaude ask` (project
// tclaude-ask, JOH-253). The out-of-box default is a balanced, capable
// model at medium effort — good for the everyday "what's the largest
// file here?", "is this diff safe?" question — with a per-call
// `-m`/`--effort` flag to drop to something cheaper/faster or reach for a
// stronger model when a question warrants it. This block is the
// persistent middle tier of that precedence (flag > this profile > the
// built-in default constants).
//
// Both fields are optional: a blank field falls back to the matching
// built-in default constant (per field, so pinning only a model keeps the
// default effort). The dashboard's Config tab edits this same block through
// its usual /api/config save flow (the Model/Effort selectors are plain
// fields of that form) — it is the single source of truth the CLI also
// reads, so the dashboard is a thin editor over config.json rather than a
// second store.
//
// The schema is harness-neutral: model/effort are validated against the
// conversation's harness catalog at ask time (ModelCatalog.ValidateModel
// / ValidateEffort), not here. Only Claude ask is wired today; Codex ask
// is a follow-up (JOH-252), so a future codex profile would validate the
// same fields against the codex catalog.
type AskConfig struct {
	// Sandbox selects current policy each turn: global (default), auto (global
	// plus the exact directory group's rules), one library profile, or off.
	Sandbox string `json:"sandbox,omitempty"`

	// Profile names a spawn profile (groups-tab profile) whose
	// harness/model/effort a FRESH ask adopts — the harness-independent way
	// to run `tclaude ask` on Codex as well as Claude (JOH-252). It is
	// resolved live at ask time (db.ResolveSpawnProfile), by primary name or
	// alias; a deleted/renamed
	// profile self-heals to the no-profile path (the Model/Effort below, then
	// the fast defaults). Only the profile's harness/model/effort are read —
	// its agent-name/role/sandbox/… fields are irrelevant to a one-shot ask
	// and ignored. "" means no profile: Claude Code, with Model/Effort below.
	Profile string `json:"profile,omitempty"`
	// Model is a model alias / full ID for ad-hoc asks, or "" to use the
	// built-in default (DefaultAskModel). Validated against the harness
	// catalog where it is consumed. Ignored when Profile is set (the
	// profile supplies the model).
	Model string `json:"model,omitempty"`
	// Effort is a reasoning-effort level for ad-hoc asks, or "" to use the
	// built-in default (DefaultAskEffort). Validated against the harness
	// catalog where it is consumed. Ignored when Profile is set (the
	// profile supplies the effort).
	Effort string `json:"effort,omitempty"`
}

// AskProfileName returns the configured ask spawn-profile name, or "" when no
// ask block / no profile is set. Nil-safe so callers need no guard.
func (c *Config) AskProfileName() string {
	if c == nil || c.Ask == nil {
		return ""
	}
	return c.Ask.Profile
}

// AskSandboxSetting is default-global even for existing configurations with
// no ask block. A named profile can use profile:<name> to escape reserved modes.
func (c *Config) AskSandboxSetting() string {
	if c == nil || c.Ask == nil || strings.TrimSpace(c.Ask.Sandbox) == "" {
		return "global"
	}
	return strings.TrimSpace(c.Ask.Sandbox)
}

func AskSandboxProfileName(setting string) (string, bool) {
	switch setting {
	case "auto", "global", "off":
		return "", false
	}
	return strings.TrimPrefix(setting, "profile:"), true
}

// DefaultAskModel / DefaultAskEffort are the built-in `tclaude ask`
// profile used when config.json pins no ask model/effort. `sonnet` at
// `medium` is a balanced, capable default for ad-hoc terminal answers —
// solid reasoning without reaching for the heaviest model, and a per-call
// `-m`/`--effort` flag for the exceptions either way. Both are aliases
// (not version-pinned IDs), so they track the latest model and stay valid
// as model names change. Kept here in ONE place so the factory default is
// a single-line change (JOH-253); they are known-good values from the
// Claude Code catalog, so a fresh config always resolves to a valid
// profile.
const (
	DefaultAskModel  = "sonnet"
	DefaultAskEffort = "medium"
)

// ResolvedAskProfile returns the effective (model, effort) for `tclaude
// ask` when no per-call flag overrides them: the configured ask.model /
// ask.effort when set, else the built-in default constants. Resolution is
// per field, so pinning only a model keeps the default effort (and vice
// versa). Nil-safe on the receiver so callers need no guard.
//
// The returned values still pass through the harness catalog's validator
// at the call site (the `tclaude ask` CLI) — this only applies the
// precedence, it does not validate.
func (c *Config) ResolvedAskProfile() (model, effort string) {
	model, effort = DefaultAskModel, DefaultAskEffort
	if c == nil || c.Ask == nil {
		return model, effort
	}
	if c.Ask.Model != "" {
		model = c.Ask.Model
	}
	if c.Ask.Effort != "" {
		effort = c.Ask.Effort
	}
	return model, effort
}

// ScribeConfig is the persistent launch profile for dashboard-summoned
// scribes (JOH-371) — the ad-hoc chat agents behind the dashboard's "Edit
// with agent" buttons (JOH-361). Out of the box a summon leaves every launch
// field unset, so a scribe falls through to the harness default (Claude Code
// at its default model/effort). This block lets the operator designate a
// saved spawn profile (a Groups-tab profile) whose whole launch shape
// (harness/model/effort/sandbox/…) a FRESH scribe adopts — e.g. to run
// scribes on Codex, or pin a cheaper model for the light editing they do.
//
// It deliberately mirrors AskConfig's precedent — reusing the SAME
// spawn-profile mechanism `tclaude ask` reuses — but is profile-only: the
// profile already carries harness/model/effort together, so there are no
// separate model/effort fields here. "" / absent = today's behaviour.
//
// Resolved LIVE at summon time (the summon re-stamps the scribe group's
// default spawn profile from this name on every summon), so a deleted
// or renamed profile self-heals to the no-profile default rather than wedging
// the summon — the same live self-heal AskConfig has. Every summon is fresh,
// so a config change applies to the next scribe without disturbing live
// scribes. The dashboard's Config tab edits this same block through the
// usual /api/config save flow, so config.json stays the single source of
// truth the summon reads.
//
// Scope: one global scribe profile for now. Per-scribe-kind profiles (a
// different profile for a future roles-scribe vs circle-scribe) are out of
// scope (JOH-362).
type ScribeConfig struct {
	// Profile names a spawn profile (Groups-tab profile) whose launch shape a
	// fresh dashboard-summoned scribe adopts. Resolved live at summon time
	// (db.ResolveSpawnProfile, via the scribe group's stamped default profile);
	// primary names and aliases are accepted, and a
	// deleted/renamed profile self-heals to the no-profile default. "" means
	// no profile: the harness default (Claude Code).
	Profile string `json:"profile,omitempty"`
}

// ScribeProfileName returns the configured scribe spawn-profile name, or ""
// when no scribe block / no profile is set. Nil-safe so callers need no guard.
// Trimmed so a hand-edited config.json with a whitespace-padded name still
// matches a real profile at resolution time (the dashboard already trims);
// otherwise a stray space would silently self-heal to the default instead of
// applying the intended profile.
func (c *Config) ScribeProfileName() string {
	if c == nil || c.Scribe == nil {
		return ""
	}
	return strings.TrimSpace(c.Scribe.Profile)
}

// StewardConfig is the access a Steward (the companion's chief of staff) is
// born with. Absent or blank fields take the defaults: operator-level reads and
// messaging. Anything beyond that goes through an approval.
type StewardConfig struct {
	// Reads is "operator" or "fleet"; see the StewardReads constants.
	Reads string `json:"reads,omitempty"`
	// Profile is the spawn profile a new Steward launches with. Empty means the
	// global default spawn profile.
	Profile string `json:"profile,omitempty"`
	// Name is what the operator calls their Steward. Empty means "the Steward".
	Name string `json:"name,omitempty"`
	// StartupPrompt is the operator's own instructions, added to the brief of
	// every new Steward.
	StartupPrompt string `json:"startup_prompt,omitempty"`
	// Writes is "none", "messaging" or "workers"; see the StewardWrites
	// constants.
	Writes string `json:"writes,omitempty"`
	// DefaultPermissions is read only to migrate it: the daemon moves these
	// slugs to the stored permissions for future Stewards when it starts, then
	// clears this. Config > Steward edits that store directly.
	DefaultPermissions []string `json:"default_permissions,omitempty"`
	// SourceDownload is "ask", "each_version" or "never"; see the
	// StewardSource constants. It is consent to a download, which is why it
	// lives here, where only the operator can set it.
	SourceDownload string `json:"source_download,omitempty"`
	// Disabled opts this instance out of stewards: no companion Steward can be
	// summoned or handed the role, the companion offers none, and external
	// stewards are refused whatever ExternalEnabled says. Every other Collab
	// surface stays on. Off by default.
	Disabled bool `json:"disabled,omitempty"`
	// ExternalEnabled lets an assistant outside tclaude act as a steward with
	// a credential the operator made. Off by default: while off, none can be
	// made and every one presented is refused.
	ExternalEnabled bool `json:"external_enabled,omitempty"`
	// ExternalMaxChildren caps the workers one external steward can have running.
	// Zero means DefaultExternalStewardMaxChildren.
	ExternalMaxChildren int `json:"external_max_children,omitempty"`
}

// DefaultExternalStewardMaxChildren is the cap when the operator hasn't set one.
const DefaultExternalStewardMaxChildren = 3

const (
	// StewardReadsOperator reads what the operator reads across tclaude.
	StewardReadsOperator = "operator"
	// StewardReadsFleet reads the fleet, the boards and the dashboard view.
	StewardReadsFleet = "fleet"

	// StewardSourceAsk asks the operator for each version with no source.
	StewardSourceAsk = "ask"
	// StewardSourceEachVersion downloads each new version's source by itself.
	StewardSourceEachVersion = "each_version"
	// StewardSourceNever downloads nothing and does not ask.
	StewardSourceNever = "never"

	// StewardWritesNone keeps only what talking to the operator needs.
	StewardWritesNone = "none"
	// StewardWritesMessaging adds messaging any agent.
	StewardWritesMessaging = "messaging"
	// StewardWritesWorkers adds starting workers and setting their task.
	StewardWritesWorkers = "workers"
)

// MaxStewardStartupPromptLen caps steward.startup_prompt in bytes, leaving a
// Steward's brief room for its built-in text within the 16 KiB initial-message
// limit whatever the alphabet.
const MaxStewardStartupPromptLen = 10000

// stewardNamePattern keeps a Steward's name inside the rename-title charset, so
// it is safe as an agent title and as a message address.
var stewardNamePattern = regexp.MustCompile(`^[A-Za-z0-9]+([ _-][A-Za-z0-9]+)*$`)

// MaxStewardNameLen caps steward.name.
const MaxStewardNameLen = 32

// StewardName returns the operator's name for their Steward, trimmed, or "".
// Nil-safe.
func (c *Config) StewardName() string {
	// Only the dashboard's save validates; a hand edit must not turn a group
	// or remote address into a Steward alias.
	name := c.stewardNameSetting()
	if !validStewardName(name) {
		return ""
	}
	return name
}

func (c *Config) stewardNameSetting() string {
	if c == nil || c.Steward == nil {
		return ""
	}
	return strings.TrimSpace(c.Steward.Name)
}

func validStewardName(name string) bool {
	return len(name) <= MaxStewardNameLen && stewardNamePattern.MatchString(name)
}

// StewardProfile returns the Steward's spawn profile, trimmed, or "". Nil-safe.
func (c *Config) StewardProfile() string {
	if c == nil || c.Steward == nil {
		return ""
	}
	return strings.TrimSpace(c.Steward.Profile)
}

// StewardStartupPrompt returns the operator's instructions, trimmed. Nil-safe.
func (c *Config) StewardStartupPrompt() string {
	if c == nil || c.Steward == nil {
		return ""
	}
	return strings.TrimSpace(c.Steward.StartupPrompt)
}

// StewardReads returns the configured read level, defaulted. Nil-safe.
func (c *Config) StewardReads() string {
	if c != nil && c.Steward != nil && c.Steward.Reads != "" {
		return c.Steward.Reads
	}
	return StewardReadsOperator
}

// StewardWrites returns the configured write level, defaulted. Nil-safe.
func (c *Config) StewardWrites() string {
	if c != nil && c.Steward != nil && c.Steward.Writes != "" {
		return c.Steward.Writes
	}
	return StewardWritesMessaging
}

// StewardsEnabled reports whether this instance has stewards at all: true
// unless the operator opted out with steward.disabled. Nil-safe.
func (c *Config) StewardsEnabled() bool {
	return c == nil || c.Steward == nil || !c.Steward.Disabled
}

// ExternalStewardsEnabled reports the external steward switch, which the
// steward opt-out overrides. Nil-safe.
func (c *Config) ExternalStewardsEnabled() bool {
	return c.StewardsEnabled() && c != nil && c.Steward != nil && c.Steward.ExternalEnabled
}

// ExternalStewardMaxChildren returns the cap on one external steward's running
// workers. Nil-safe.
func (c *Config) ExternalStewardMaxChildren() int {
	if c == nil || c.Steward == nil || c.Steward.ExternalMaxChildren <= 0 {
		return DefaultExternalStewardMaxChildren
	}
	return c.Steward.ExternalMaxChildren
}

// StewardSourceDownload returns the operator's choice on downloading tclaude's
// source for the knowledge base, defaulted. Nil-safe.
func (c *Config) StewardSourceDownload() string {
	if c != nil && c.Steward != nil && c.Steward.SourceDownload != "" {
		return c.Steward.SourceDownload
	}
	return StewardSourceAsk
}

// StewardDefaultPermissions returns the slugs still to migrate, trimmed.
// Nil-safe.
func (c *Config) StewardDefaultPermissions() []string {
	if c == nil || c.Steward == nil {
		return nil
	}
	var slugs []string
	for _, slug := range c.Steward.DefaultPermissions {
		if slug = strings.TrimSpace(slug); slug != "" {
			slugs = append(slugs, slug)
		}
	}
	return slugs
}

// CostConfig holds cost display knobs and estimated API spend alert limits.
//
// EstimateFactor is an opt-in multiplier applied to every *displayed*
// cost figure (the per-agent cost badge, the Costs tab, and the top
// bar's month-to-date / today readouts). Claude Code computes its cost
// from token counts client-side and flags it as an estimate; in
// practice that estimate runs a little below the actual billed amount,
// so a factor of e.g. 1.1 nudges the displayed numbers up ~10% to track
// reality.
//
// It is purely a display multiplier. The values stored in the DB
// (sessions.cost_usd, session_cost_daily) are never scaled, so changing
// the factor only changes what the dashboard shows, never recorded
// history — toggling it back to 1 restores the raw figures exactly.
//
// nil block / nil pointer / a non-positive value all mean "no
// adjustment" (factor 1.0). An out-of-range value is clamped by
// ResolvedCostFactor and reported by Validate.
//
// ShowOnSubscription opts a SUBSCRIPTION account into the dashboard's Costs
// tab. On pay-per-token the tab always shows (there's real spend); on a
// subscription there's no real charge, so by default the tab auto-hides. Set
// this true to reveal harness-provided pay-per-token-equivalent estimates from
// virtual_cost_usd, clearly flagged as hypothetical and kept distinct from
// real spend. Default false = hide subscription estimates. Editable from the
// dashboard's Config tab.
//
// GeminiBilling classifies Gemini CLI cost, which tclaude prices itself from
// the session file's token counts (Gemini records no dollar figure): "auto"
// (the default, also "") reads Gemini's selected auth type — an API key or
// Vertex AI is billed per token and shows as real spend, a Google sign-in is
// shown as a WHAT-IF estimate; "api" and "subscription" force either side.
type CostConfig struct {
	// Spend limits use unscaled API ledger estimates; zero disables a period.
	DailyLimitUSD   float64  `json:"daily_limit_usd,omitempty"`
	MonthlyLimitUSD float64  `json:"monthly_limit_usd,omitempty"`
	EstimateFactor  *float64 `json:"estimate_factor,omitempty"`
	// HarnessFactors replace EstimateFactor for a harness. An explicit 1 is
	// meaningful: it disables adjustment even when the default is higher.
	HarnessFactors     map[string]float64 `json:"harness_factors,omitempty"`
	ShowOnSubscription bool               `json:"show_on_subscription,omitempty"`
	GeminiBilling      string             `json:"gemini_billing,omitempty"`
}

// Gemini billing classifications (cost.gemini_billing).
const (
	GeminiBillingAuto         = "auto"
	GeminiBillingAPI          = "api"
	GeminiBillingSubscription = "subscription"
)

// ResolvedGeminiBilling returns the configured Gemini billing classification,
// GeminiBillingAuto when unset or unknown. Nil-safe on the receiver.
func (c *Config) ResolvedGeminiBilling() string {
	if c == nil || c.Cost == nil {
		return GeminiBillingAuto
	}
	switch v := strings.TrimSpace(c.Cost.GeminiBilling); v {
	case GeminiBillingAPI, GeminiBillingSubscription:
		return v
	default:
		return GeminiBillingAuto
	}
}

// defaultCostFactor is the no-op multiplier: the displayed cost equals
// the recorded cost.
const defaultCostFactor = 1.0

// maxCostEstimateFactor is the upper bound on the display multiplier. A
// compensation factor lives just above 1 (≈1.1 for the observed ~10%
// gap); a far larger value is almost certainly a fat-finger (e.g. "110"
// meant as a percent), so the editor rejects it and the resolver clamps
// it rather than letting it 100×-inflate the dashboard.
const maxCostEstimateFactor = 10.0

// ResolvedCostFactor returns the effective display multiplier for cost
// figures: 1.0 when unconfigured, the configured value otherwise,
// clamped to (0, maxCostEstimateFactor]. A nil config / absent block /
// non-positive value all yield 1.0 (no adjustment); an over-range value
// is clamped down so a hand-edited absurd value cannot silently blow up
// the display (mirrors ResolvedSlopVolumes). Nil-safe on the receiver.
func (c *Config) ResolvedCostFactor() float64 {
	if c == nil || c.Cost == nil || c.Cost.EstimateFactor == nil {
		return defaultCostFactor
	}
	f := *c.Cost.EstimateFactor
	if f <= 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return defaultCostFactor
	}
	if f > maxCostEstimateFactor {
		return maxCostEstimateFactor
	}
	return f
}

// CostFactorForHarness resolves an override, otherwise the global default.
// Unknown or missing harness identities use the default; OpenCode's factor
// applies to every provider within that harness.
func (c *Config) CostFactorForHarness(name string) float64 {
	if c != nil && c.Cost != nil {
		if f, ok := c.Cost.HarnessFactors[name]; ok && f > 0 && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return min(f, maxCostEstimateFactor)
		}
	}
	return c.ResolvedCostFactor()
}

// ValidCostFactorHarness identifies the supported per-harness settings.
func ValidCostFactorHarness(name string) bool {
	switch name {
	case "claude", "codex", "opencode", "copilot", "gemini", "pi":
		return true
	default:
		return false
	}
}

// Empty reports whether the optional cost block can be omitted.
func (c *CostConfig) Empty() bool {
	return c == nil || (c.DailyLimitUSD == 0 && c.MonthlyLimitUSD == 0 && c.EstimateFactor == nil && len(c.HarnessFactors) == 0 && !c.ShowOnSubscription && c.GeminiBilling == "")
}

// OpenCodeConfig tunes tclaude's OpenCode integration.
type OpenCodeConfig struct {
	// LegacyLongContextPricingCutoff is the per-call context-token boundary
	// above which OpenCode's legacy experimentalOver200K catalog price is
	// selected. Explicit catalog context tiers remain authoritative. Nil,
	// zero, and negative values resolve to the built-in default so a stale or
	// hand-edited config cannot move the boundary to an unsafe value.
	LegacyLongContextPricingCutoff *int64 `json:"legacy_long_context_pricing_cutoff,omitempty"`
}

// DefaultOpenCodeLegacyLongContextPricingCutoff is the current OpenAI
// long-context boundary used for OpenCode catalogs that still expose only the
// legacy experimentalOver200K price shape.
const DefaultOpenCodeLegacyLongContextPricingCutoff int64 = 272_000

// ResolvedOpenCodeLegacyLongContextPricingCutoff returns the effective
// per-call token boundary for OpenCode's legacy long-context price fallback.
// It is deliberately forgiving for runtime reads: absent, zero, or negative
// values use the documented default. Validate reports explicit non-positive
// values to the dashboard editor instead of silently accepting them.
func (c *Config) ResolvedOpenCodeLegacyLongContextPricingCutoff() int64 {
	if c == nil || c.OpenCode == nil || c.OpenCode.LegacyLongContextPricingCutoff == nil ||
		*c.OpenCode.LegacyLongContextPricingCutoff <= 0 {
		return DefaultOpenCodeLegacyLongContextPricingCutoff
	}
	return *c.OpenCode.LegacyLongContextPricingCutoff
}

// UsageConfig tunes the dashboard's subscription-usage readout (the 5h/7d
// bars in the top bar).
type UsageConfig struct {
	// IdleTimeout is how long the last-known Claude usage reading stays
	// visible after fresh data stops arriving, as a Go duration string
	// ("72h", "30m", "2h30m"). The Claude readout is fed by two sources:
	// Claude Code's statusline callback (only while a session runs) and,
	// only when PollAnthropicAPI is enabled, a periodic Anthropic usage-API
	// poll. A failed poll preserves the cached figures but does NOT advance
	// their freshness clock, so a short cap would hide a perfectly good 7d
	// (weekly) reading the same night, leaving only Codex on the top bar.
	// This is the grace window: keep showing the last-known reading for this
	// long since the last successful update, then degrade to "usage: n/a".
	// Empty / absent → DefaultUsageIdleTimeout; a value that doesn't parse,
	// or is ≤ 0, is rejected by Validate. See ResolvedUsageIdleTimeout.
	IdleTimeout string `json:"idle_timeout,omitempty"`

	// PollAnthropicAPI opts agentd into periodically refreshing the Claude
	// subscription usage cache via Anthropic's OAuth usage API. Disabled by
	// default: Claude Code's statusline callback already supplies the same
	// data while sessions run, and background API polling can hit 429 rate
	// limits on otherwise idle machines. Enable only when you want dashboard
	// usage bars refreshed while no Claude Code statusline callback is active.
	PollAnthropicAPI bool `json:"poll_anthropic_api,omitempty"`
}

// DefaultUsageIdleTimeout is how long a last-known Claude usage reading
// stays on the dashboard after its live source goes dark, when config.json
// pins no usage.idle_timeout. Three days comfortably outlives an overnight
// idle spell where no Claude Code statusline callback is running, so the 5h/7d
// bars persist off the last good reading instead of collapsing to "usage: n/a"
// — while still eventually clearing a genuinely dead source.
const DefaultUsageIdleTimeout = 72 * time.Hour

// ResolvedUsageIdleTimeout returns the effective grace window for the
// dashboard's Claude usage readout: the configured usage.idle_timeout when
// it parses to a positive duration, else DefaultUsageIdleTimeout. Nil-safe
// on the receiver, and forgiving of a blank / garbage value (Validate is
// what surfaces a bad string to the human; the resolver never leaves the
// readout unbounded).
func (c *Config) ResolvedUsageIdleTimeout() time.Duration {
	if c == nil || c.Usage == nil || c.Usage.IdleTimeout == "" {
		return DefaultUsageIdleTimeout
	}
	d, err := time.ParseDuration(c.Usage.IdleTimeout)
	if err != nil || d <= 0 {
		return DefaultUsageIdleTimeout
	}
	return d
}

// PollAnthropicUsageAPI reports whether agentd should periodically call the
// Anthropic OAuth usage API to refresh Claude subscription usage. The default
// is false so the dashboard relies on Claude Code's statusline callback and
// cached figures unless the user explicitly opts into background API polling.
func (c *Config) PollAnthropicUsageAPI() bool {
	return c != nil && c.Usage != nil && c.Usage.PollAnthropicAPI
}

// ConvWatchConfig holds the watch view's persisted UI preferences.
type ConvWatchConfig struct {
	// Columns is the set of explicit column-visibility overrides, keyed by
	// column key ("harness", "project", "size", "modified", "groups"). A
	// key present here shadows that column's smart auto-default (e.g.
	// HARNESS auto-shows only when a non-Claude conv is present); an absent
	// key follows the auto rule. Written by the in-view column selector
	// (the `c` overlay); unknown keys are ignored by readers.
	Columns map[string]bool `json:"columns,omitempty"`
}

// SessionWatchConfig holds the live sessions view's persisted UI preferences.
type SessionWatchConfig struct {
	// Columns is the set of explicit column-visibility overrides. An absent
	// key follows that column's default; unknown keys are ignored by readers.
	// Written by the in-view `c` column selector.
	Columns map[string]bool `json:"columns,omitempty"`
}

// SlopConfig holds the slop-mode audio knobs. Both volumes are percent
// (0–100) of the mode's built-in full level: MusicVolume scales the
// Vegas lounge radio, EffectsVolume scales the synthesized casino FX.
// Pointers so "absent" (music defaults to 50, effects to 100) is
// distinguishable from an explicit 0 (silent but not muted — the master
// 🔇/🔊 switch is a separate localStorage-persisted preference in the
// browser).
//
// Channel is the SomaFM channel id the Vegas radio tunes to (one of
// SlopChannels; absent → DefaultSlopChannel). A pointer + omitempty so an
// untouched config stays clean and an absent value is the default rather
// than the empty string.
//
// VegasInRegularMode, when true, surfaces the Vegas music features — the
// Vegas tab, the header volume mixer (🎚️) and master sound switch (🔊),
// and the lounge radio — on the PLAIN dashboard, not just in slop
// ("casino") mode. It decouples the soundtrack from the full cosmetic
// re-skin: you get music + volume + the tab WITHOUT the slot machines,
// header shimmer, coins and sound FX. A *bool so absent = off (the
// features stay slop-only) and an explicit value round-trips through the
// Config tab.
//
// HidePullLever, when true, hides the slop-mode side pull-lever — the
// casino lever pinned to the right edge of the Groups tab that spins every
// machine at once. Slop mode otherwise stays fully intact; this just drops
// that one ornament for people who find it in the way. A *bool so absent =
// off (the lever shows, the historical default) and an explicit value
// round-trips through the Config tab.
//
// Written by the dashboard's volume sliders via POST /api/slop/volumes and
// the channel picker via POST /api/slop/channel; also round-trips through
// the Config tab like any other field.
type SlopConfig struct {
	MusicVolume        *int    `json:"music_volume,omitempty"`
	EffectsVolume      *int    `json:"effects_volume,omitempty"`
	Channel            *string `json:"channel,omitempty"`
	VegasInRegularMode *bool   `json:"vegas_in_regular_mode,omitempty"`
	HidePullLever      *bool   `json:"hide_pull_lever,omitempty"`
}

// SlopChannels is the allowlist of SomaFM channel ids the dashboard radio
// can tune to. It is the SINGLE SOURCE OF TRUTH shared by config validation
// (here), the now-playing proxy's SSRF gate (agentd), and the browser's
// channel picker (js/vegas.js carries a matching catalog with human labels,
// pinned to this set by TestSlopNowPlaying_ChannelMatchesVegasJS).
//
// The radio is theme-agnostic: this one flat allowlist backs both the 🎰
// slop/Vegas soundtrack and the 🧙 wizard soundtrack. The browser groups
// these ids into "Vegas Lounge" vs "Wizard's Realm" for its two-level
// picker (js/vegas.js's CHANNELS carry the group), but the server only
// cares that a requested id is allowlisted — the group is a pure UI filter.
//
// Adding a channel is a one-line entry here plus a matching {id,label,group}
// in vegas.js. Every other URL (stream, station home, songs feed) derives
// from the id by SomaFM's fixed URL shape, so the id is all that's shared.
var SlopChannels = []string{
	// Vegas Lounge group — the original slop-mode soundtrack.
	"illstreet",   // Illinois Street Lounge — vintage cocktail / Rat-Pack
	"secretagent", // Secret Agent — spy-jazz & surf
	"groovesalad", // Groove Salad — ambient / downtempo
	"lush",        // Lush — mostly vocal, mostly chilled
	"bootliquor",  // Boot Liquor — americana roots
	"u80s",        // Underground 80s — early alternative / new wave
	"defcon",      // DEF CON Radio — music for hacking
	// Wizard's Realm group — fantasy-flavored SomaFM channels for 🧙 mode.
	"thistle",      // ThistleRadio — Celtic roots ("The Tavern", wizard default)
	"folkfwd",      // Folk Forward — indie / alt-folk ("The Bard's Rest")
	"dronezone",    // Drone Zone — atmospheric ambient ("The Astral Plane")
	"darkzone",     // The Dark Zone — dark ambient ("The Dungeon")
	"doomed",       // Doomed — dark industrial ambient ("The Crypt")
	"deepspaceone", // Deep Space One — deep-space ambient ("The Cosmos")
}

// DefaultWizardChannel is the station the wizard soundtrack tunes to for a
// fresh listener (one who has never explicitly picked a channel). It is the
// Celtic "Tavern" — the closest thing SomaFM has to a fantasy tavern. The
// server treats it like any other allowlisted id; only the browser knows it
// is the wizard group's default (see js/vegas.js). Kept here beside the
// allowlist so the two never drift.
const DefaultWizardChannel = "thistle"

// DefaultSlopChannel is the channel the Vegas radio plays when none is
// configured — the original vintage lounge, so a fresh config keeps the
// historical soundtrack.
const DefaultSlopChannel = "illstreet"

// IsKnownSlopChannel reports whether id is in the SlopChannels allowlist.
func IsKnownSlopChannel(id string) bool {
	return slices.Contains(SlopChannels, id)
}

// HasExplicitSlopChannel reports whether the config carries a real, known
// channel choice — as opposed to falling back to DefaultSlopChannel because
// nothing was set. The dashboard radio uses this to tell a fresh listener
// (who should hear the active theme's default station — the Tavern in wizard
// mode) apart from someone who deliberately picked a station. Nil-safe on the
// receiver.
func (c *Config) HasExplicitSlopChannel() bool {
	if c == nil || c.Slop == nil || c.Slop.Channel == nil {
		return false
	}
	return IsKnownSlopChannel(strings.TrimSpace(*c.Slop.Channel))
}

// ResolvedSlopChannel returns the effective channel id: the configured one
// when it's a known channel, else DefaultSlopChannel. A hand-edited unknown
// id degrades to the default here (Validate reports it to the Config tab),
// so readers always get a streamable channel. Nil-safe on the receiver.
func (c *Config) ResolvedSlopChannel() string {
	if c == nil || c.Slop == nil || c.Slop.Channel == nil {
		return DefaultSlopChannel
	}
	id := strings.TrimSpace(*c.Slop.Channel)
	if IsKnownSlopChannel(id) {
		return id
	}
	return DefaultSlopChannel
}

// DefaultMusicVolume is the effective music volume (the Vegas/wizard lounge
// radio) for an absent slop.music_volume key: 50% — full-volume slop and
// wizard mode startled users on first entry, so the soundtrack defaults to
// half. `tclaude setup` also writes this value explicitly so it's visible in
// the config / Config tab (see setup.installDefaultMusicVolume); readers fall
// back to it here for any config that predates that write or was hand-cleared.
const DefaultMusicVolume = 50

// defaultEffectsVolume is the effective volume for an absent
// slop.effects_volume key: 100%. The synthesized casino FX only fire on
// interaction (not a continuous stream like the radio), so full volume there
// isn't the part that startled anyone — only the music default was lowered.
const defaultEffectsVolume = 100

// ResolvedSlopVolumes returns the effective (music, effects) volumes in
// percent: an absent music volume defaults to DefaultMusicVolume (50) and an
// absent effects volume to 100. A hand-edited out-of-range value is clamped
// to 0–100 — Validate reports it to the Config tab, but readers must still
// get a usable volume rather than handing 500% to the browser. Nil-safe on
// the receiver so callers need no guard.
func (c *Config) ResolvedSlopVolumes() (music, effects int) {
	music, effects = DefaultMusicVolume, defaultEffectsVolume
	if c == nil || c.Slop == nil {
		return music, effects
	}
	if c.Slop.MusicVolume != nil {
		music = min(100, max(0, *c.Slop.MusicVolume))
	}
	if c.Slop.EffectsVolume != nil {
		effects = min(100, max(0, *c.Slop.EffectsVolume))
	}
	return music, effects
}

// ShowVegasInRegularMode reports whether the Vegas music features (the
// Vegas tab, the header volume mixer + sound switch, and the lounge
// radio) should appear on the plain dashboard — config
// slop.vegas_in_regular_mode. Off by default (absent / nil / explicit
// false); only an explicit true opts in. Nil-safe on the receiver so
// callers need no guard.
func (c *Config) ShowVegasInRegularMode() bool {
	if c == nil || c.Slop == nil || c.Slop.VegasInRegularMode == nil {
		return false
	}
	return *c.Slop.VegasInRegularMode
}

// HidePullLever reports whether the slop-mode side pull-lever (the casino
// lever pinned to the right edge of the Groups tab) should be hidden —
// config slop.hide_pull_lever. Off by default (absent / nil / explicit
// false), so the lever shows as it historically did; only an explicit true
// hides it. Nil-safe on the receiver so callers need no guard.
func (c *Config) HidePullLever() bool {
	if c == nil || c.Slop == nil || c.Slop.HidePullLever == nil {
		return false
	}
	return *c.Slop.HidePullLever
}

// normalizeActivityBotsStyle returns s when it's a known style, else ""
// (so resolvers fall back to their per-mode default for a blank or a
// hand-edited garbage value).
func normalizeActivityBotsStyle(s string) string {
	switch s {
	case ActivityBotsEmoji, ActivityBotsSprites, ActivityBotsOff:
		return s
	default:
		return ""
	}
}

// ActivityBotsRegular reports the activity-bot style for the plain
// (non-slop) dashboard — config dashboard.activity_bots.regular. Default
// "emoji" (absent block/key or an unknown value). Nil-safe on the receiver.
func (c *Config) ActivityBotsRegular() string {
	if c != nil && c.Dashboard != nil && c.Dashboard.ActivityBots != nil {
		if s := normalizeActivityBotsStyle(c.Dashboard.ActivityBots.Regular); s != "" {
			return s
		}
	}
	return ActivityBotsEmoji
}

// ActivityBotsSlop reports the activity-bot style for slop ("casino") mode
// — config dashboard.activity_bots.slop. Default "sprites" (absent block/key
// or an unknown value). Nil-safe on the receiver.
func (c *Config) ActivityBotsSlop() string {
	if c != nil && c.Dashboard != nil && c.Dashboard.ActivityBots != nil {
		if s := normalizeActivityBotsStyle(c.Dashboard.ActivityBots.Slop); s != "" {
			return s
		}
	}
	return ActivityBotsSprites
}

// ActivityBotsWizard reports the activity-bot style for wizard mode — config
// dashboard.activity_bots.wizard. Default "emoji", which the wizard wrapper
// renders as its fantasy-glyph row; "sprites" opts into the WIZARD spellcaster
// sheets instead, and "off" hides it. Absent block/key or an unknown value
// falls back to the default. Nil-safe on the receiver.
func (c *Config) ActivityBotsWizard() string {
	if c != nil && c.Dashboard != nil && c.Dashboard.ActivityBots != nil {
		if s := normalizeActivityBotsStyle(c.Dashboard.ActivityBots.Wizard); s != "" {
			return s
		}
	}
	return ActivityBotsEmoji
}

// ShowPluginsTabAlways reports whether the dashboard should keep the Plugins
// tab visible even with no plugins installed — config
// dashboard.always_show_plugins_tab. Default false (the tab auto-hides when
// the installed set is empty). Nil-safe on the receiver.
func (c *Config) ShowPluginsTabAlways() bool {
	return c != nil && c.Dashboard != nil && c.Dashboard.AlwaysShowPluginsTab
}

// ShowAgentHideButton reports whether each agent row's "hide window" button
// (the slashed-eye beside "focus") should be shown — config
// dashboard.show_agent_hide_button. Default false: the button is hidden to
// keep the quick-control cluster tight ("focus" stays); only an explicit
// true brings it back. Nil-safe on the receiver.
func (c *Config) ShowAgentHideButton() bool {
	return c != nil && c.Dashboard != nil && c.Dashboard.ShowAgentHideButton
}

// ShowGroupDescription reports whether each group header's 📝 description chip
// should be shown — config dashboard.show_group_description. Default false: the
// group-description feature is deprecated (display-only, drives nothing), so
// the chip is hidden to keep headers tight; only an explicit true brings it
// back. Nil-safe on the receiver.
func (c *Config) ShowGroupDescription() bool {
	return c != nil && c.Dashboard != nil && c.Dashboard.ShowGroupDescription
}

// ShowFinalPRChecks reports whether merged/closed PRs keep their CI checks
// pill even when the final run is not failing — config
// dashboard.show_final_pr_checks. Default false. Nil-safe on the receiver.
func (c *Config) ShowFinalPRChecks() bool {
	return c != nil && c.Dashboard != nil && c.Dashboard.ShowFinalPRChecks
}

// keybindingIDPattern and keybindingChordPattern bound dashboard.keybindings
// to the registry's id shape and to modifier+KeyboardEvent.code chords.
var (
	keybindingIDPattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)
	keybindingChordPattern = regexp.MustCompile(`^((Ctrl|Meta|Alt|Shift|Mod)\+){0,5}[A-Za-z0-9-]{1,32}$`)
)

// maxKeybindings caps dashboard.keybindings well above the registry's size.
const maxKeybindings = 256

func validKeybinding(id, chord string) bool {
	return keybindingIDPattern.MatchString(id) && keybindingChordPattern.MatchString(chord)
}

// DashboardKeybindings returns the operator's shortcut overrides from
// dashboard.keybindings, dropping any entry whose id or chord is malformed.
// Nil when there are none. Nil-safe on the receiver.
func (c *Config) DashboardKeybindings() map[string]string {
	if c == nil || c.Dashboard == nil || len(c.Dashboard.Keybindings) == 0 {
		return nil
	}
	out := make(map[string]string, len(c.Dashboard.Keybindings))
	for id, chord := range c.Dashboard.Keybindings {
		if validKeybinding(id, chord) {
			out[id] = chord
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// StayOnTerminalsAfterLastClose reports whether the dashboard should stay on
// the Terminals tab when its last open terminal closes — config
// dashboard.stay_on_terminals_after_last_close. Default false: the dashboard
// moves to the Groups tab. Nil-safe on the receiver.
func (c *Config) StayOnTerminalsAfterLastClose() bool {
	return c != nil && c.Dashboard != nil && c.Dashboard.StayOnTerminalsAfterLastClose
}

// FusedToolbarFixedNode reports whether the fused Groups view's toolbar stays
// on the node picked in its node chip — config
// dashboard.fused_toolbar_fixed_node. Default false: it follows the node of
// the group last acted on. Nil-safe on the receiver.
func (c *Config) FusedToolbarFixedNode() bool {
	return c != nil && c.Dashboard != nil && c.Dashboard.FusedToolbarFixedNode
}

// UsageCollapseSubviews reports whether the Usage tab collapses to its only
// populated view (Quota or Costs) and drops the view switcher — config
// dashboard.usage_collapse_subviews. Default false: Overview, Quota and Costs
// are always offered. Nil-safe on the receiver.
func (c *Config) UsageCollapseSubviews() bool {
	return c != nil && c.Dashboard != nil && c.Dashboard.UsageCollapseSubviews
}

// HScrollFollow reports whether the dashboard's full-bleed chrome bars
// should keep their content pinned to the viewport (follow mode) while the
// page is scrolled sideways — config dashboard.hscroll_follow. Default true
// (absent block / nil pointer); only an explicit "hscroll_follow": false
// selects static mode. Nil-safe on the receiver so callers need no guard.
func (c *Config) HScrollFollow() bool {
	if c == nil || c.Dashboard == nil || c.Dashboard.HScrollFollow == nil {
		return true
	}
	return *c.Dashboard.HScrollFollow
}

// RecentPRWindowDaysDefault / RecentPRWindowDaysMax bound the footer
// popover's "recently closed" lookback — config
// dashboard.recent_pr_window_days.
const (
	RecentPRWindowDaysDefault = 3
	RecentPRWindowDaysMax     = 30
)

// VoiceConfig is dashboard.voice. A new spoken command is one more
// VoicePhraseDefaults entry, and its phrase lands in the same list.
type VoiceConfig struct {
	// Phrases lists spoken phrases by action. An action left out keeps its
	// default, and an empty phrase or Enabled false turns that action off.
	Phrases []VoicePhrase `json:"phrases,omitempty"`
	// WakeEnabled opts in to the always-on listener. It has nothing to listen
	// for unless the wake phrase is on too; neither setting changes the other.
	WakeEnabled bool `json:"wake_enabled,omitempty"`
	// SilenceSendSeconds opts in to sending a dictated message after this many
	// seconds with no new speech. 0 is off.
	SilenceSendSeconds int `json:"silence_send_seconds,omitempty"`
	// MarkDictated prefixes dictated messages to agents with a short marker,
	// so the agent knows why punctuation may be missing. Default on.
	MarkDictated *bool `json:"mark_dictated,omitempty"`
	// CommaPauseSeconds and PeriodPauseSeconds opt in to punctuation from
	// pauses in dictation: a pause this long adds a comma, or a period that
	// capitalizes what follows. 0 is off.
	CommaPauseSeconds  float64 `json:"comma_pause_seconds,omitempty"`
	PeriodPauseSeconds float64 `json:"period_pause_seconds,omitempty"`
	// ContinuousEnabled opts in to continuous mode: after a voice message
	// sends, the next spoken sentence sends another with no wake phrase.
	ContinuousEnabled bool `json:"continuous_enabled,omitempty"`
	// ContinuousIdleSeconds ends continuous mode after this much silence.
	ContinuousIdleSeconds int `json:"continuous_idle_seconds,omitempty"`
}

// VoicePhrase is one spoken phrase and what it does.
type VoicePhrase struct {
	Action string `json:"action"`
	Phrase string `json:"phrase"`
	// Enabled is whether the phrase is active. Nil keeps the action's default.
	// It is stored apart from Phrase so turning a phrase off keeps its text.
	Enabled *bool `json:"enabled,omitempty"`
}

// Voice phrase actions.
const (
	VoiceActionSend = "send"
	VoiceActionWake = "wake"
	VoiceActionStop = "stop"
	// VoiceActionSteward wakes the Steward, whatever has focus.
	VoiceActionSteward = "steward"
)

// VoicePhraseDefaults are the known actions in display order, with neutral
// defaults. The wake phrase avoids words people say to their agents.
var VoicePhraseDefaults = []VoicePhrase{
	{Action: VoiceActionSend, Phrase: "send now", Enabled: new(true)},
	{Action: VoiceActionWake, Phrase: "hey agent", Enabled: new(false)},
	{Action: VoiceActionSteward, Phrase: "hey stewie", Enabled: new(false)},
	{Action: VoiceActionStop, Phrase: "stop listening", Enabled: new(true)},
}

// DictationSendPhraseDefault, the phrase cap and the silence-send range bound
// config dashboard.voice.
const (
	DictationSendPhraseDefault   = "send now"
	DictationSendPhraseMaxRunes  = 60
	SilenceSendSecondsMin        = 2
	SilenceSendSecondsMax        = 30
	VoicePauseSecondsMin         = 0.3
	VoicePauseSecondsMax         = 10.0
	ContinuousIdleSecondsDefault = 120
	ContinuousIdleSecondsMin     = 30
	ContinuousIdleSecondsMax     = 3600
)

func cleanVoicePhrase(phrase string) string {
	runes := []rune(strings.Join(strings.Fields(phrase), " "))
	return strings.TrimSpace(string(runes[:min(len(runes), DictationSendPhraseMaxRunes)]))
}

// Voice resolves dashboard.voice for its readers: every known action with its
// cleaned phrase, "" when it is off, the wake listener only when it has an
// active wake phrase of its own, and the silence send clamped into range. A
// wake phrase equal to an active send phrase would send and wake on the same
// words, so it counts as off, as does a Steward phrase equal to either and a
// stop phrase equal to any of them. Nil-safe on the receiver.
func (c *Config) Voice() VoiceConfig {
	var stored VoiceConfig
	if c != nil && c.Dashboard != nil && c.Dashboard.Voice != nil {
		stored = *c.Dashboard.Voice
	}
	out := VoiceConfig{
		WakeEnabled:           stored.WakeEnabled,
		MarkDictated:          new(stored.MarkDictated == nil || *stored.MarkDictated),
		ContinuousEnabled:     stored.ContinuousEnabled,
		ContinuousIdleSeconds: ContinuousIdleSecondsDefault,
	}
	if stored.ContinuousIdleSeconds > 0 {
		out.ContinuousIdleSeconds = max(ContinuousIdleSecondsMin, min(stored.ContinuousIdleSeconds, ContinuousIdleSecondsMax))
	}
	if stored.SilenceSendSeconds > 0 {
		out.SilenceSendSeconds = max(SilenceSendSecondsMin, min(stored.SilenceSendSeconds, SilenceSendSecondsMax))
	}
	if stored.CommaPauseSeconds > 0 {
		out.CommaPauseSeconds = max(VoicePauseSecondsMin, min(stored.CommaPauseSeconds, VoicePauseSecondsMax))
	}
	if stored.PeriodPauseSeconds > 0 {
		out.PeriodPauseSeconds = max(VoicePauseSecondsMin, min(stored.PeriodPauseSeconds, VoicePauseSecondsMax))
	}
	phrases := map[string]string{}
	enabled := map[string]bool{}
	for _, known := range VoicePhraseDefaults {
		phrases[known.Action] = known.Phrase
		enabled[known.Action] = *known.Enabled
	}
	phrases[VoiceActionSend] = c.DictationSendPhrase()
	for _, phrase := range stored.Phrases {
		if _, known := phrases[phrase.Action]; known {
			phrases[phrase.Action] = cleanVoicePhrase(phrase.Phrase)
			if phrase.Enabled != nil {
				enabled[phrase.Action] = *phrase.Enabled
			}
		}
	}
	for action, on := range enabled {
		if !on {
			phrases[action] = ""
		}
	}
	if strings.EqualFold(phrases[VoiceActionWake], phrases[VoiceActionSend]) {
		phrases[VoiceActionWake] = ""
	}
	if steward := phrases[VoiceActionSteward]; strings.EqualFold(steward, phrases[VoiceActionSend]) || strings.EqualFold(steward, phrases[VoiceActionWake]) {
		phrases[VoiceActionSteward] = ""
	}
	stop := phrases[VoiceActionStop]
	if strings.EqualFold(stop, phrases[VoiceActionSend]) || strings.EqualFold(stop, phrases[VoiceActionWake]) || strings.EqualFold(stop, phrases[VoiceActionSteward]) {
		phrases[VoiceActionStop] = ""
	}
	for _, known := range VoicePhraseDefaults {
		out.Phrases = append(out.Phrases, VoicePhrase{Action: known.Action, Phrase: phrases[known.Action]})
	}
	if phrases[VoiceActionWake] == "" && phrases[VoiceActionSteward] == "" {
		out.WakeEnabled = false
	}
	return out
}

// DictationSendPhrase reports the spoken send phrase, with its whitespace
// collapsed. Default DictationSendPhraseDefault (absent block / nil pointer),
// "" means spoken send is off, and anything past the rune cap is cut so a
// hand-edited config stays displayable. Nil-safe on the receiver.
func (c *Config) DictationSendPhrase() string {
	if c == nil || c.Dashboard == nil || c.Dashboard.DictationSendPhrase == nil {
		return DictationSendPhraseDefault
	}
	return cleanVoicePhrase(*c.Dashboard.DictationSendPhrase)
}

// AlwaysShowOpenPRs reports whether the dashboard footer's Open PRs
// indicator stays mounted with zero open pull requests — config
// dashboard.always_show_open_prs. Default true (absent block / nil pointer);
// only an explicit "always_show_open_prs": false restores the show-only-when-
// non-empty behaviour. Nil-safe on the receiver so callers need no guard.
func (c *Config) AlwaysShowOpenPRs() bool {
	if c == nil || c.Dashboard == nil || c.Dashboard.AlwaysShowOpenPRs == nil {
		return true
	}
	return *c.Dashboard.AlwaysShowOpenPRs
}

// RecentPRWindowDays reports the lookback, in days, for the footer popover's
// "recently closed" pull-request filter — config
// dashboard.recent_pr_window_days. Default RecentPRWindowDaysDefault (absent
// block / nil pointer), 0 means the filter is off, and anything above
// RecentPRWindowDaysMax (or negative) is clamped into range. Nil-safe on the
// receiver so callers need no guard.
func (c *Config) RecentPRWindowDays() int {
	if c == nil || c.Dashboard == nil || c.Dashboard.RecentPRWindowDays == nil {
		return RecentPRWindowDaysDefault
	}
	days := *c.Dashboard.RecentPRWindowDays
	if days < 0 {
		return 0
	}
	return min(days, RecentPRWindowDaysMax)
}

// normalizeGroupQuickOptions returns s when it's a known mode, else ""
// (so the resolver falls back to its default for a blank or hand-edited
// garbage value).
func normalizeGroupQuickOptions(s string) string {
	switch s {
	case GroupQuickOptionsHover, GroupQuickOptionsExpanded:
		return s
	default:
		return ""
	}
}

// OverlayBackdrop returns dashboard.overlay_backdrop, with "dim" for anything
// unset or unknown.
func (c *Config) OverlayBackdrop() string {
	if c != nil && c.Dashboard != nil {
		switch v := c.Dashboard.OverlayBackdrop; v {
		case "blur", "both", "none":
			return v
		}
	}
	return "dim"
}

// SnapAssist reports whether a space a snap leaves empty offers the other
// overlay windows. Default true; dashboard.no_snap_assist turns it off.
func (c *Config) SnapAssist() bool {
	return c == nil || c.Dashboard == nil || !c.Dashboard.NoSnapAssist
}

// WindowControls returns dashboard.window_controls, with "auto" for anything
// unset or unknown.
func (c *Config) WindowControls() string {
	if c != nil && c.Dashboard != nil {
		switch v := c.Dashboard.WindowControls; v {
		case "standard", "os", "macos", "windows", "linux", "classic":
			return v
		}
	}
	return "auto"
}

// GroupQuickOptions reports the display mode for the group <summary> quick-
// option chips — config dashboard.group_quick_options. Default "hover"
// (absent block / key or an unknown value): the chips fold to icon-only at
// rest and expand on header hover. "expanded" keeps them always visible.
// Nil-safe on the receiver so callers need no guard.
func (c *Config) GroupQuickOptions() string {
	if c != nil && c.Dashboard != nil {
		if s := normalizeGroupQuickOptions(c.Dashboard.GroupQuickOptions); s != "" {
			return s
		}
	}
	return GroupQuickOptionsHover
}

// normalizeDefaultTerminal returns s when it's a known mode, else "" (so the
// resolver falls back to its default for a blank or hand-edited garbage value).
func normalizeDefaultTerminal(s string) string {
	switch s {
	case DefaultTerminalNative, DefaultTerminalWeb:
		return s
	default:
		return ""
	}
}

// DefaultTerminal reports how the dashboard's spawn auto-focus, per-agent
// focus / open-window / open-terminal actions open a console — config
// dashboard.default_terminal.
// Default "web" (absent block / key or an unknown value): route actions to an
// in-browser terminal pane in the dashboard's Terminals tab. "native" opts
// back into an OS window (with the usual in-browser fallback when none can be
// opened). Nil-safe on the receiver so callers need no guard.
func (c *Config) DefaultTerminal() string {
	if c != nil && c.Dashboard != nil {
		if s := normalizeDefaultTerminal(c.Dashboard.DefaultTerminal); s != "" {
			return s
		}
	}
	return DefaultTerminalWeb
}

// DefaultDirectoryPicker reports the configured directory chooser for local
// dashboard connections. Remote browser connections override this to "web"
// client-side because a native dialog would appear on the agentd host.
func (c *Config) DefaultDirectoryPicker() string {
	if c != nil && c.Dashboard != nil {
		switch c.Dashboard.DefaultDirectoryPicker {
		case DefaultDirectoryPickerNative, DefaultDirectoryPickerWeb:
			return c.Dashboard.DefaultDirectoryPicker
		}
	}
	return DefaultDirectoryPickerWeb
}

// FocusConfig holds window-focus behavior knobs.
//
// RaiseOnly, when true, makes window-focus RAISE an existing terminal
// window only — it never opens a fresh one as a side effect. Default
// (false) keeps the historical behavior: focusing an agent that has no
// attached client opens a new terminal running `tclaude session attach`
// (what macOS does too). Opt-in for permissive compositors where the
// open-on-focus fallback pops up / raises a window unexpectedly on every
// dashboard "show" that resolves to a detached agent. The explicit
// dashboard "open window" action opens a console regardless of this flag.
//
// Tile configures the opt-in auto-tiling pass that runs after a bulk
// window "focus" op (the 🪟 windows… modal, the command palette, or a
// group's focus button) — see TileConfig. Absent / disabled (the
// default) leaves each terminal wherever the OS placed it.
//
// WindowTitle gates whether tclaude stamps the `tclaude:<id>` window/tab
// title on each pane (the tmux set-titles pair in session.runNew + the OSC
// escape in AttachToSession). That title is how the WSL and native-Linux/X11
// focus + tiling paths locate an agent's existing window to raise it; it's
// also what some users find "ugly" on a plain desktop terminal. A *bool so
// absent is distinguishable from an explicit false: nil / true → stamp the
// title (the default, keeps focus-by-title working); explicit false → skip
// both emit sites entirely, so the terminal keeps its own title. Turning it
// off degrades "focus/raise the existing window" to "open a new window"
// wherever focus is title-based (WSL, native-Linux/X11) and disables
// auto-tiling; the explicit dashboard "open window" action is unaffected.
// See WindowTitleEnabled.
type FocusConfig struct {
	RaiseOnly   bool        `json:"raise_only,omitempty"`
	Tile        *TileConfig `json:"tile,omitempty"`
	WindowTitle *bool       `json:"window_title,omitempty"`
}

// Tile layout modes — config focus.tile.layout. "grid" packs windows
// into a near-square grid (the default); "columns" lays them out as
// full-height side-by-side columns; "rows" as full-width stacked rows;
// "cascade" overlaps them with a fixed diagonal step (macOS-style stagger).
// An empty / unknown value falls back to the default (grid) — see TileLayout.
const (
	TileLayoutGrid    = "grid"
	TileLayoutColumns = "columns"
	TileLayoutRows    = "rows"
	TileLayoutCascade = "cascade"
)

// TileConfig configures the auto-tiling pass. When Enabled, a bulk
// window "focus" op that raises/opens more than one window follows up by
// arranging just that focused set into the chosen Layout, so the desktop
// is neatly tiled instead of leaving every window where the OS dropped
// it. All focused windows are gathered onto ONE monitor — the monitor the
// first window is on — so a multi-monitor setup doesn't scatter them or
// straddle the gap. It is best-effort and platform-specific (AppleScript
// on macOS, xdotool/kdotool on native Linux, PowerShell on WSL); an
// unsupported desktop simply no-ops. A single-window focus is never tiled
// (there is nothing to arrange).
//
// Resize controls whether windows are RESIZED to fill the layout. The
// default (false) keeps each window at its current size and only
// repositions it so the set no longer overlaps — the least-intrusive
// "just line them up" behaviour. Set it true for the older screen-filling
// grid, where each window is stretched to fill its layout cell.
//
// Gap is the pixel spacing left between adjacent tiles; Margin is the
// pixel inset kept from the screen work-area edges (useful to clear a
// menu bar / panel the platform's screen query doesn't already exclude).
// Both are pointers so "absent" is distinguishable from an explicit 0:
// nil falls back to the built-in default (defaultTileGap / defaultTileMargin),
// an explicit 0 means flush. See ResolvedTileGeometry.
type TileConfig struct {
	Enabled bool   `json:"enabled,omitempty"`
	Layout  string `json:"layout,omitempty"`
	Resize  bool   `json:"resize,omitempty"`
	Gap     *int   `json:"gap,omitempty"`
	Margin  *int   `json:"margin,omitempty"`
}

// Tiling geometry defaults + the sanity cap Validate/ResolvedTileGeometry
// enforce. An 8px gap gives a visible seam between tiled terminals; the
// default margin is 0 (the platform screen query already excludes the
// dock/taskbar in the common cases). maxTilePixels bounds a hand-edited
// gap/margin so a fat-finger can't shrink every tile to nothing or push
// the whole grid off-screen.
const (
	defaultTileGap    = 8
	defaultTileMargin = 0
	maxTilePixels     = 1000
)

// RaiseOnlyFocus reports whether window focus should be raise-only (raise
// an existing window but never open a fresh one). Nil-safe so callers
// need no guard; the absent default is false (open-on-focus).
func (c *Config) RaiseOnlyFocus() bool {
	if c == nil || c.Focus == nil {
		return false
	}
	return c.Focus.RaiseOnly
}

// WindowTitleEnabled reports whether tclaude should stamp the `tclaude:<id>`
// window/tab title on its panes — config focus.window_title. Default true
// (absent block / key, or an explicit true): the title is on, so the WSL and
// native-Linux/X11 focus-by-title + tiling paths can find an agent's window.
// An explicit false skips the title so a plain desktop terminal keeps its own
// tab title (at the cost of title-based focus/tiling). Nil-safe so callers
// need no guard.
func (c *Config) WindowTitleEnabled() bool {
	if c == nil || c.Focus == nil || c.Focus.WindowTitle == nil {
		return true
	}
	return *c.Focus.WindowTitle
}

// TileOnFocus reports whether a bulk window "focus" op should follow up
// by auto-tiling the focused windows — config focus.tile.enabled. Off by
// default (absent block / key). Nil-safe on the receiver.
func (c *Config) TileOnFocus() bool {
	return c != nil && c.Focus != nil && c.Focus.Tile != nil && c.Focus.Tile.Enabled
}

// normalizeTileLayout returns s when it's a known layout, else "" (so the
// resolver falls back to its default for a blank or hand-edited garbage
// value).
func normalizeTileLayout(s string) string {
	switch s {
	case TileLayoutGrid, TileLayoutColumns, TileLayoutRows, TileLayoutCascade:
		return s
	default:
		return ""
	}
}

// TileResize reports whether the tiling pass should RESIZE windows to
// fill their layout cells — config focus.tile.resize. Default false
// (absent block/key): windows keep their current size and are only
// repositioned. Nil-safe on the receiver.
func (c *Config) TileResize() bool {
	return c != nil && c.Focus != nil && c.Focus.Tile != nil && c.Focus.Tile.Resize
}

// TileLayout reports the tiling layout mode — config focus.tile.layout.
// Default "grid" (absent block/key or an unknown value). Nil-safe on the
// receiver.
func (c *Config) TileLayout() string {
	if c != nil && c.Focus != nil && c.Focus.Tile != nil {
		if l := normalizeTileLayout(c.Focus.Tile.Layout); l != "" {
			return l
		}
	}
	return TileLayoutGrid
}

// ResolvedTileGeometry returns the effective (gap, margin) in pixels for
// the tiling pass, defaulting each absent value to the built-in default
// and clamping a hand-edited out-of-range value to [0, maxTilePixels] so
// readers always get a usable geometry (Validate reports the out-of-range
// value to the Config tab). Nil-safe on the receiver.
func (c *Config) ResolvedTileGeometry() (gap, margin int) {
	gap, margin = defaultTileGap, defaultTileMargin
	if c == nil || c.Focus == nil || c.Focus.Tile == nil {
		return gap, margin
	}
	t := c.Focus.Tile
	if t.Gap != nil {
		gap = min(maxTilePixels, max(0, *t.Gap))
	}
	if t.Margin != nil {
		margin = min(maxTilePixels, max(0, *t.Margin))
	}
	return gap, margin
}

// LogRotationConfig holds the agentd log-rotation knobs. agentd caps
// the active log (~/.tclaude/output.log) at MaxSize and keeps Keep
// rotated files (output.log.1 … output.log.<Keep>), dropping the
// oldest. Every tclaude process appends to the log; only agentd
// rotates. See pkg/common/logrotate.go and agentd/logrotate.go.
//
// The struct is nested (rather than two flat Config keys) so a future
// time/date-based rotation mode can be added here — e.g. a "mode" or
// "max_age" field — without reshaping config.json.
type LogRotationConfig struct {
	// MaxSize is the active-log size cap as a human-friendly string
	// parsed by common.ParseSize, e.g. "10MiB", "50m", "500k". Empty
	// means the built-in default (10 MiB). An explicit "0" is a valid
	// zero size and disables rotation entirely.
	MaxSize string `json:"max_size,omitempty"`

	// Keep is how many rotated files to retain. <= 0 means the
	// built-in default (5).
	Keep int `json:"keep,omitempty"`
}

// Log-rotation defaults — used when config.json omits the keys or
// gives an unparseable value. 10 MiB is large enough that rotation is
// rare yet small enough that a rotated file stays openable; keeping 5
// rotated files preserves roughly 50 MiB of recent history.
const (
	defaultLogMaxSize int64 = 10 * common.MB
	defaultLogKeep          = 5
)

// ResolvedLogRotation returns the effective (maxSizeBytes, keep) for
// agentd's log rotation. A nil config, an absent log_rotation block, or
// an omitted/blank max_size all yield the built-in defaults. An
// explicit max_size of "0" (a valid zero size) yields maxSizeBytes 0,
// which the caller treats as "rotation disabled". An unparseable
// max_size also falls back to the default — Validate surfaces it so a
// human editing config through the dashboard is told.
//
// It is nil-safe on the receiver so callers need no guard.
func (c *Config) ResolvedLogRotation() (maxSizeBytes int64, keep int) {
	maxSizeBytes, keep = defaultLogMaxSize, defaultLogKeep
	if c == nil || c.LogRotation == nil {
		return maxSizeBytes, keep
	}
	lr := c.LogRotation
	if lr.Keep > 0 {
		keep = lr.Keep
	}
	if lr.MaxSize != "" {
		if n, err := common.ParseSize(lr.MaxSize); err == nil {
			maxSizeBytes = n
		}
	}
	return maxSizeBytes, keep
}

// AgentConfig holds agent-coordination knobs.
//
// DefaultPermissions are granted to every agent — baseline trust the
// human curates by hand. Per-agent overrides used to live here too,
// but moved to SQLite (table agent_permissions) in v9: the daemon
// rewrites them through grant/revoke endpoints, and storing them in
// JSON made round-tripping awkward (config.json is hand-edited for
// log_level etc.). config keeps only what humans naturally write.
//
// Permission slugs are simple dotted strings, e.g. "self.rename",
// "groups.members.update", "agent.spawn". Unknown slugs are ignored
// (forward-compat: a user grants a permission a future build wires up).
//
// Sudo carries the human-curated defaults for `tclaude agent sudo`
// (time-bounded permission elevations). Hand-written; the daemon
// reads but never rewrites it. Empty fields fall back to the agentd
// hardcoded defaults. Per-caller overrides via Sudo.Overrides[] use
// selector-shaped keys (conv-id / stable `agt_` agent-id / title, with
// prefix match) like the historical permission_overrides block did.
// An `agt_`-tagged key survives the caller's reincarnate / /clear
// rotation where a conv-id key would go stale.
//
// AutoLaunchDashboard, when true, makes `tclaude agentd serve` open the
// browser dashboard on startup — the persistent twin of the
// --auto-launch-dashboard serve flag. The flag and this field OR
// together: either one turns it on, so an autostart/service launch can
// opt in without carrying the flag.
//
// CloneCooldown is the minimum cooldown between two clones of the same
// source agent — a Go duration string ("1m", "30s"). It is the
// persistent twin of the `tclaude agentd serve --agent-clone-cooldown`
// flag, which overrides it when set; the built-in default is 1m. "0"
// disables the cooldown. An unparseable value is warned about and
// ignored, falling through to the flag/default. The cooldown applies
// only to agent-initiated clones — human-initiated clones (CLI or
// dashboard) are never rate-limited.
//
// SpawnGroupRestriction / SpawnAllowedGroups / SpawnMaxPerHour are the
// global knobs of the agent-spawn guardrail layer — runaway-prevention
// for the case where the human grants an AGENT the `groups.members.spawn`
// permission. They only ever affect agent callers; a human (no claude
// ancestor) bypasses every spawn guardrail, as everywhere else.
//
//   - SpawnGroupRestriction toggles the group restriction: when on
//     (the default — a nil pointer means on), an agent may only spawn
//     into a group it is itself a member or owner of. Set it to false
//     to let a spawn-capable agent spawn into any group.
//   - SpawnAllowedGroups widens the restriction with a fixed allowlist
//     of group names an agent may always spawn into, even when it is
//     not a member/owner. Empty (the default) means no extra groups.
//   - SpawnMaxPerHour caps how many agents one caller-agent may spawn
//     per rolling hour. A nil pointer means the built-in default (10);
//     0 disables the rate limit (unlimited). The daemon resolves it
//     into agentd.SpawnMaxPerWindow once at startup.
//
// (CloneCooldown above is a distinct, separately-named knob — the
// clone cooldown — not part of this guardrail layer.)
//
// The per-group member cap is NOT here — it is a hard property of the
// group itself (agent_groups.max_members), set via `groups
// set-max-members` / the dashboard, and applies to every caller.
//
// BranchHistoryPREnrichment, when true, lets the dashboard's
// branch-link resolver stamp the PR it resolves (number/URL/state)
// onto the conv_branch_history rows. Off by default: v1 of the
// branch-history feature records the *branches* an agent worked on but
// leaves the PR columns empty, until a caching strategy for the
// branch→PR mapping lands. The branch re-scan and the PostToolUse hook
// append run regardless of this flag — neither ever shells out to gh;
// only the PR stamp is gated. See branchlinks.go.
type AgentConfig struct {
	DefaultPermissions              []string            `json:"default_permissions,omitempty"`
	Sudo                            *SudoConfig         `json:"sudo,omitempty"`
	ContextNudge                    *ContextNudgeConfig `json:"context_nudge,omitempty"`
	AutoLaunchDashboard             bool                `json:"auto_launch_dashboard,omitempty"`
	AccessRequestAutoOpenBrowser    bool                `json:"access_request_auto_open_browser,omitempty"`
	AccessRequestSystemNotification bool                `json:"access_request_system_notification,omitempty"`
	PresentPRNotification           bool                `json:"present_pr_notification,omitempty"`
	DisableTray                     bool                `json:"disable_tray,omitempty"` // suppress the agentd tray icon; --no-tray ORs with it
	BranchHistoryPREnrichment       bool                `json:"branch_history_pr_enrichment,omitempty"`
	CloneCooldown                   string              `json:"clone_cooldown,omitempty"`
	// ResourceDelegationDir selects a cgroup v2 root delegated to an external,
	// long-lived tmux runtime unit. agentd serve's flag and environment
	// override it; empty preserves the legacy self-cgroup derivation.
	ResourceDelegationDir string   `json:"resource_delegation_dir,omitempty"`
	SpawnGroupRestriction *bool    `json:"spawn_group_restriction,omitempty"`
	SpawnAllowedGroups    []string `json:"spawn_allowed_groups,omitempty"`
	SpawnMaxPerHour       *int     `json:"spawn_max_per_hour,omitempty"`

	// Morph tunes cross-harness morph history translation.
	Morph *MorphConfig `json:"morph,omitempty"`

	// RetiredCleanup is the opt-in long-horizon auto-cleanup that fully
	// DELETES agents/conversations that have been retired for a very long
	// time (JOH-269). Absent / disabled (the default) keeps today's
	// keep-retired-forever behaviour — retire stays the non-destructive
	// half of cleanup. See RetiredCleanupConfig + ResolvedRetiredCleanup.
	RetiredCleanup *RetiredCleanupConfig `json:"retired_cleanup,omitempty"`

	// SpawnLegacyInjection reverts the daemon's Claude Code spawn flow to the
	// legacy path: launch a bare `claude`, poll for its conv-id, then inject
	// `/rename <name>` and the welcome turn over tmux with delays. The default
	// (absent / false) uses the faster launch-enrollment path — `claude
	// --session-id --name <prompt>` — which names + greets the agent at launch
	// with no post-connect tmux injection. Set it true to fall back if the
	// launch-arg path ever misbehaves. No effect on harnesses that don't
	// support launch enrollment (Codex always uses the inject-after-connect
	// flow). See agentd.spawnUsesLegacyInjection.
	SpawnLegacyInjection *bool `json:"spawn_legacy_injection,omitempty"`

	// SpawnNameNormalize controls whether the spawn surfaces auto-normalize
	// an entered agent name to the safe [A-Za-z0-9_-] branch-token charset
	// (collapsing spaces/punctuation/unicode to '-', e.g. "code reviewer!" →
	// "code-reviewer") instead of rejecting it with a 400. It is a *bool so
	// the default-on state (nil / absent) is distinguishable from an explicit
	// off: nil means ON — any typed name "just works", which is the
	// out-of-box behaviour the dashboard's spawn modal, `tclaude agent spawn`,
	// and the daemon's spawn boundary all share. Set it false to restore the
	// strict reject-invalid-name behaviour. See agent.NormalizeSpawnName and
	// Config.SpawnNameNormalizeEnabled.
	SpawnNameNormalize *bool `json:"spawn_name_normalize,omitempty"`

	// SpawnLabelFromName derives a spawned agent's session label — the
	// sessions-table PK, and therefore the tmux session name `session new`
	// renders from it — from the agent's --name instead of the historical
	// random "spwn-XXXXXX" token. So an agent named "code-reviewer" attaches
	// as `tclaude session attach code-reviewer` and shows up under that name
	// in `tmux ls` / the status line.
	//
	// The name is the BASE CANDIDATE, not necessarily the final label. It is
	// run through agent.NormalizeSpawnName ("café" → "caf") and trimmed to
	// agent.MaxSpawnNameLen minus the longest suffix the disambiguation tiers
	// can append, so a disambiguated label still clears the same 64-char cap
	// the name itself passed.
	//
	// Default OFF (absent / false) keeps the random token: a name-derived
	// label is not globally unique, so a taken base is disambiguated with a
	// `-2`, `-3`, … suffix (the same shape `session new` uses for a taken tmux
	// name) — and those suffixes climb for as long as the older namesake's
	// session row survives, because a session id owns durable per-session
	// history (costs, telemetry, notify state) that must never be reused.
	// Operators who prefer stable-but-opaque labels should leave this off.
	//
	// The numeric ladder is bounded; past it the base keeps a random hex
	// suffix ("worker-a3f9c1"), and past THAT the label degrades to a plain
	// random token so a spawn never fails over its cosmetic label. A spawn
	// with no name, or whose name normalizes to nothing, takes that random
	// token straight away. See agentd.spawnLabelSequence for the exact tiers.
	SpawnLabelFromName bool `json:"spawn_label_from_name,omitempty"`

	// SpawnInlineMaxChars bounds the "inline the briefing into the first turn"
	// optimisation. When a freshly-spawned agent's startup briefing (group
	// context + task brief) fits within this many runes, the whole briefing is
	// baked into the launch prompt right after the [system: ...] welcome — so the
	// agent acts on its first turn instead of running a `tclaude agent inbox read
	// <id>` round-trip first. A longer briefing keeps the pointer welcome and
	// stays in the inbox (scrollable, doesn't bloat the launch command / first
	// turn). The briefing is ALWAYS also saved to the inbox either way; inlining
	// only changes whether the first turn carries it. nil →
	// DefaultSpawnInlineMaxChars; <= 0 disables inlining (always pointer).
	//
	// Governs both harnesses: Claude Code's launch-enrollment prompt and Codex's
	// conv-id seed (see agentd.buildSpawnSeedPrompt) both honour it. The Codex
	// wrinkle: Codex has no conv-id — and so no inbox-message id — at launch, so
	// an inlined Codex seed omits the "(also saved to inbox #N)" note and a long
	// Codex briefing's pointer welcome is injected post-connect rather than at
	// launch. Has no effect on the legacy send-keys path (CC's
	// spawn_legacy_injection revert), where the welcome must stay a single line
	// (a newline = an early submit). See agentd.spawnInlineMaxChars.
	SpawnInlineMaxChars *int `json:"spawn_inline_max_chars,omitempty"`

	// MessageInlineMaxChars bounds safe mailbox messages that are included
	// directly in their pane nudge. Newlines and tabs are delivered through a
	// bracketed tmux paste so they remain part of one submitted turn. The inbox
	// row remains the durable archive; an inlined copy is atomically marked
	// delivered and read. Longer or control-bearing messages keep the
	// traditional inbox-read pointer.
	// nil uses DefaultMessageInlineMaxChars; <= 0 disables regular-message
	// inlining without affecting spawn briefings.
	MessageInlineMaxChars *int `json:"message_inline_max_chars,omitempty"`

	// DashboardPort pins the loopback TCP port the agentd dashboard +
	// human-approval popup bind to. 0 / absent (the default) lets the OS
	// pick a random free port at each `agentd serve`. A fixed port gives
	// a stable, bookmarkable URL (and lets the dashboard's per-browser
	// prefs persist across restarts, since localStorage is keyed by
	// origin). The `agentd serve --dashboard-port` flag overrides this.
	// A configured port already in use (or out of range) fails daemon
	// startup rather than silently falling back to a random port — that
	// would break whatever the fixed port was set up for. See
	// agentd.resolveDashboardPort.
	DashboardPort int `json:"dashboard_port,omitempty"`

	// DashboardBind sets an additional host/interface the agentd dashboard +
	// human-approval server binds to. It is a HOST only (no port — the port
	// is DashboardPort / --dashboard-port); e.g. "127.0.0.1", "0.0.0.0",
	// "::", or a specific interface IP. A concrete non-loopback bind is
	// additive: the same port remains available on 127.0.0.1.
	//
	// Empty / absent (the default) means "127.0.0.1" — loopback only, the
	// historical behaviour: the dashboard + approval popup are reachable
	// only from this machine. Set a non-loopback host to additionally expose
	// the dashboard on the network so an EXTERNALLY-managed auth layer (a
	// reverse proxy, VPN, mesh, Cloudflare Access, …) can front it. That
	// is the intended use — the dashboard's own gate is only a cookie +
	// operator token, so binding it wide WITHOUT your own auth in front
	// would expose it to anyone who can reach the port. When bound
	// non-loopback, agentd relaxes its same-origin check from the fixed
	// loopback URL to host-relative (Origin.Host == request Host), the
	// same model the remote (mTLS) listener uses, so a browser reaching
	// the dashboard through any hostname/IP still works while the
	// SameSite=Strict cookie keeps CSRF closed.
	//
	// The `agentd serve --dashboard-bind` flag overrides this. An invalid
	// host fails daemon startup (and the config editor's Validate catches
	// it earlier with a friendly message). See agentd.resolveDashboardBind.
	DashboardBind string `json:"dashboard_bind,omitempty"`

	// PersistOperatorToken opts the daemon into a STABLE operator token
	// that survives restarts, instead of the default (a fresh random
	// token minted in memory each `agentd serve` and lost on exit).
	//
	// Off / absent (the default) preserves the historical behaviour: the
	// human re-reads the token off the startup banner and re-exports
	// TCLAUDE_HUMAN_TOKEN after every daemon restart. On, the token is
	// generated once and persisted, so the human exports it a single time
	// and it keeps working across restarts. The `agentd serve
	// --persist-operator-token` flag ORs with this — either turns it on.
	//
	// The persisted secret is stored in a 0600
	// ~/.tclaude/data/operator_token file by default.
	// The secret is deliberately NOT held in this config file — config.json
	// is plaintext and shows up in the Config-tab diff / backups, and the
	// agent sandbox already denies reads to ~/.tclaude/data.
	PersistOperatorToken bool `json:"persist_operator_token,omitempty"`

	// PersistOperatorTokenKeychain explicitly selects the OS keychain instead
	// of the private file for persistent operator-token storage. It implies
	// persistence even when PersistOperatorToken is false. Keychain access is
	// not a portable agent-sandbox boundary, so it is opt-in rather than the
	// default. Existing values are never copied between the two stores.
	PersistOperatorTokenKeychain bool `json:"persist_operator_token_keychain,omitempty"`

	// GitProxy configures the daemon-mediated Git-remote / GitHub proxy —
	// `tclaude proxy git` and `tclaude proxy github`. Absent (the default)
	// means the proxy is OFF: it is an opt-in surface that lends agentd's
	// credentials to a sandboxed agent, so it must never come up enabled on
	// an operator who has not configured it. See GitProxyConfig.
	GitProxy *GitProxyConfig `json:"git_proxy,omitempty"`

	// LinearProxy configures the daemon-mediated Linear proxy — `tclaude
	// proxy linear`. Absent (the default) means the proxy is OFF, for the
	// same reason GitProxy is: it lends the operator's Linear credential to
	// a sandboxed agent, so it must never come up enabled on an operator who
	// has not configured it. See LinearProxyConfig.
	LinearProxy *LinearProxyConfig `json:"linear_proxy,omitempty"`

	// AWBProxy configures the daemon-mediated AWB proxy — `tclaude proxy
	// awb`. Absent (the default) means the proxy is OFF, for the same reason
	// GitProxy and LinearProxy are: it lends the operator's AWB account to a
	// sandboxed agent, so it must never come up enabled on an operator who has
	// not configured it. See AWBProxyConfig.
	AWBProxy *AWBProxyConfig `json:"awb_proxy,omitempty"`

	// HTTPProxies are named credential-bearing HTTP services, available only
	// through proxy.http grants (optionally scoped by http_proxy name).
	HTTPProxies        map[string]HTTPProxyConfig `json:"http_proxies,omitempty"`
	ModelProxyDisabled bool                       `json:"model_proxy_disabled,omitempty"`
}

// MorphConfig tunes how a cross-harness morph carries history.
type MorphConfig struct {
	// HistoryBudgetPercent is the share of the target model's context window
	// the translated history may fill (default 60); the oldest turns beyond
	// it are left to the transcript file.
	HistoryBudgetPercent int `json:"history_budget_percent,omitempty"`
	// ToolResultClipBytes bounds each carried tool call input and result
	// (default 16384); the full text stays in the transcript file.
	ToolResultClipBytes int `json:"tool_result_clip_bytes,omitempty"`
}

// AccessRequestAutoOpenBrowser reports whether an agent-triggered
// --ask-human approval request should raise/open the local browser. Off by
// default: pending requests already surface inside the dashboard's Messages tab.
func (c *Config) AccessRequestAutoOpenBrowser() bool {
	return c != nil && c.Agent != nil && c.Agent.AccessRequestAutoOpenBrowser
}

// AccessRequestSystemNotification reports whether a pending --ask-human
// approval request should also raise an OS notification. This is an opt-in
// request-specific knob; the global notifications.enabled master switch is
// still enforced by the notification sender.
func (c *Config) AccessRequestSystemNotification() bool {
	return c != nil && c.Agent != nil && c.Agent.AccessRequestSystemNotification
}

// PresentPRNotification reports whether a `tclaude agent present-pr`
// presentation should also raise an OS notification. Off by default: a
// presented PR already surfaces on the agent's dashboard row. The global
// notifications.enabled master switch is still enforced by the
// notification sender.
func (c *Config) PresentPRNotification() bool {
	return c != nil && c.Agent != nil && c.Agent.PresentPRNotification
}

// DefaultSpawnInlineMaxChars is the fallback briefing-inline threshold (runes)
// used when AgentConfig.SpawnInlineMaxChars is unset. Roughly a few paragraphs
// — long enough to inline a typical short task brief on the first turn, short
// enough that a genuinely large brief still routes to the inbox rather than
// ballooning the launch command. See agentd.spawnInlineMaxChars.
const DefaultSpawnInlineMaxChars = 2000

// DefaultMessageInlineMaxChars matches the startup-brief threshold: the live
// pane path uses a bounded bracketed paste for multiline content, so regular
// operator, peer, and system instructions share one product-level cutoff.
const DefaultMessageInlineMaxChars = DefaultSpawnInlineMaxChars

// ContextNudgeConfig controls the opt-in "consider reincarnating"
// nudge that fires as a long-running agent's context fills. Off by
// default — a fresh daemon shouldn't start typing into the agent's
// pane until the human signs up for it.
//
// Threshold ladder: starting at MinPct, every IntervalPct, capped at
// 90. So MinPct=30 + IntervalPct=10 → fires at 30, 40, 50, 60, 70,
// 80, 90. MinPct=50 + IntervalPct=20 → 50, 70, 90.
//
// tclaude tracks per-session "highest threshold already fired"
// in sessions.nudged_pct so flicker around a boundary doesn't
// re-fire. ResetCompact zeroes it so a compacted session can be
// re-nudged on its next climb.
type ContextNudgeConfig struct {
	Enabled     bool `json:"enabled,omitempty"`
	MinPct      int  `json:"min_pct,omitempty"`
	IntervalPct int  `json:"interval_pct,omitempty"`
}

// defaultContextNudgeMinPct / defaultContextNudgeIntervalPct are the
// fallbacks when Enabled is true but the user didn't specify a
// threshold ladder. Picked to fire often enough to be useful (30%
// is the first "we're past the easy zone" moment) without spamming
// (10-point steps give six nudges max over a session).
const (
	defaultContextNudgeMinPct      = 30
	defaultContextNudgeIntervalPct = 10
)

// Resolved returns the effective (MinPct, IntervalPct) for this
// config — caller-supplied values when present, sensible defaults
// otherwise. Enabled callers should use this so they don't have to
// repeat the fallback logic. Returns zeros when Enabled is false
// so the caller can tell "off" apart from "on with defaults".
func (c *ContextNudgeConfig) Resolved() (enabled bool, minPct, intervalPct int) {
	if c == nil || !c.Enabled {
		return false, 0, 0
	}
	minPct = c.MinPct
	if minPct <= 0 {
		minPct = defaultContextNudgeMinPct
	}
	intervalPct = c.IntervalPct
	if intervalPct <= 0 {
		intervalPct = defaultContextNudgeIntervalPct
	}
	return true, minPct, intervalPct
}

// RetiredCleanupConfig controls the opt-in long-horizon auto-cleanup
// that permanently DELETES agents/conversations once they have been
// retired for AfterDays days (JOH-269). It is the general retention
// lever on top of retire: retire demotes an agent to a plain
// conversation but keeps its row + .jsonl forever (the non-destructive
// half of cleanup); this is what eventually reclaims that disk + DB
// growth for entities nobody reinstated.
//
// Off by default — deleting is irreversible, so a fresh daemon must
// never start removing conversations until the human explicitly opts
// in. Deleting a conversation does NOT lose its dollar cost:
// session_cost_daily denormalises conv_id at write time, so spend
// totals survive (the row just reverts to its "(unknown)" title once
// conv_index is gone). The default window is deliberately long
// (DefaultRetiredCleanupAfterDays, ~1 year) so anything still wanted
// has long since been reinstated or referenced.
type RetiredCleanupConfig struct {
	// Enabled turns the sweep on. Default false (absent) keeps the
	// historical keep-retired-forever behaviour.
	Enabled bool `json:"enabled,omitempty"`
	// AfterDays is how many days a conversation must have been retired
	// before it is eligible for deletion. 0 / absent means the built-in
	// default (DefaultRetiredCleanupAfterDays) — see ResolvedRetiredCleanup.
	AfterDays int `json:"after_days,omitempty"`
}

// DefaultRetiredCleanupAfterDays is the out-of-box retention window the
// sweep uses when RetiredCleanup is enabled but pins no AfterDays — ~1
// year. Long enough that a still-wanted retired conversation has been
// reinstated or referenced well before it is reaped.
const DefaultRetiredCleanupAfterDays = 365

// MaxRetiredCleanupAfterDays caps the retention window at ~100 years.
// No real retention policy approaches it; the cap exists purely to keep an
// absurd hand-edited value (e.g. order 1e18) from overflowing the day
// arithmetic in time.AddDate and wrapping the cutoff into the FUTURE —
// which would make every retired conversation immediately eligible. Both
// ResolvedRetiredCleanup (the runtime path, which never calls Validate)
// and Validate enforce it, so a hand-edited config is safe even though
// only the dashboard save runs Validate.
const MaxRetiredCleanupAfterDays = 36525

// ResolvedRetiredCleanup returns whether the long-horizon retired-agent
// cleanup is enabled and, if so, the effective retention window in days.
// Nil-safe so callers need no guard. Returns (false, 0) when the block is
// absent or disabled, so a caller can tell "off" apart from "on with the
// default window". A non-positive AfterDays resolves to the built-in
// default — never a zero/negative window, which would make every retired
// conversation immediately eligible — and an over-large value is clamped to
// MaxRetiredCleanupAfterDays so the cutoff can never overflow into the future.
func (c *Config) ResolvedRetiredCleanup() (enabled bool, afterDays int) {
	if c == nil || c.Agent == nil || c.Agent.RetiredCleanup == nil || !c.Agent.RetiredCleanup.Enabled {
		return false, 0
	}
	afterDays = c.Agent.RetiredCleanup.AfterDays
	if afterDays <= 0 {
		afterDays = DefaultRetiredCleanupAfterDays
	}
	if afterDays > MaxRetiredCleanupAfterDays {
		afterDays = MaxRetiredCleanupAfterDays
	}
	return true, afterDays
}

// PreCompactGuardConfig controls the PreCompact hook that refuses an
// auto-compaction while the conversation's used context is still below
// a per-window-size floor. Its purpose is to stop Claude Code from
// compacting a 1M-context session at the 200K boundary (CC's default
// for non-extended models, which fires at ~20% of the 1M status bar):
// the guard lets context accrue to a chosen level — at which point the
// operator typically reincarnates — before compaction is allowed.
//
// It only ever PREVENTS an early compaction; it never forces one. The
// guard fails OPEN: when it is disabled, or the data needed to judge
// (the session's stored context snapshot) is missing, or no threshold
// matches the conversation's window, compaction is allowed.
// BrokerConfig controls agentd's brokered endpoints — the path by which
// an agent whose mount namespace hides the conversation database
// (`tclaude-layer`) applies its hook events and statusline writes through
// the daemon instead.
//
// The limits it governs are a denial-of-service backstop, not traffic
// shaping. Real traffic is a statusline that re-renders several times a
// second plus a tool-use burst on top, and the ceilings sit far above
// that; a caller reaching them is malfunctioning or hostile, not busy.
//
// Enforcement is deliberately OPT-IN. With this block absent the limiter
// still measures every caller and logs anything over the line, saying
// what it WOULD have refused — so an operator can see real traffic
// against the ceilings before deciding to turn rejection on, rather than
// discovering the sizing was wrong by having a working agent cut off.
type BrokerConfig struct {
	// EnforceLimits turns rejection on. Off (the default) is shadow
	// mode: measured, logged, never refused.
	EnforceLimits bool `json:"enforce_limits,omitempty"`
}

// BrokerLimitsEnforced reports whether the brokered endpoints should
// actually refuse a caller over the line, as opposed to logging what they
// would have refused.
func (c *Config) BrokerLimitsEnforced() bool {
	return c != nil && c.Broker != nil && c.Broker.EnforceLimits
}

type PreCompactGuardConfig struct {
	// Enabled turns the guard on. Off (the default) installs the
	// PreCompact hook but always allows compaction, so toggling this
	// at runtime needs no hook re-install.
	Enabled bool `json:"enabled"`
	// BlockManual also guards a manual `/compact` (trigger="manual").
	// Default false: only Claude Code's automatic compaction is
	// refused, never a compaction the human typed themselves.
	BlockManual bool `json:"block_manual,omitempty"`
	// Thresholds maps a context-window size (tokens) to the minimum
	// used-context (tokens) required before auto-compaction is allowed
	// on that window. Empty → DefaultPreCompactThresholds.
	Thresholds []PreCompactThreshold `json:"thresholds,omitempty"`
}

// PreCompactThreshold is one (window, floor) pair: on a context window
// of WindowSize tokens, compaction is refused until used context
// reaches MinTokens.
type PreCompactThreshold struct {
	WindowSize int64 `json:"window_size"`
	MinTokens  int64 `json:"min_tokens"`
}

// DefaultPreCompactThresholds is the built-in floor ladder used when
// the guard is enabled but no thresholds are configured: hold off
// auto-compaction until 150K/200K (75%) on a standard window and
// 800K/1M (80%) on an extended window.
func DefaultPreCompactThresholds() []PreCompactThreshold {
	return []PreCompactThreshold{
		{WindowSize: 200_000, MinTokens: 150_000},
		{WindowSize: 1_000_000, MinTokens: 800_000},
	}
}

// ResolvedThresholds returns the effective floor ladder — the
// configured thresholds when present, the built-in defaults otherwise.
// Returns nil when the guard is nil or disabled so callers can tell
// "off" from "on with defaults".
func (g *PreCompactGuardConfig) ResolvedThresholds() []PreCompactThreshold {
	if g == nil || !g.Enabled {
		return nil
	}
	if len(g.Thresholds) > 0 {
		return g.Thresholds
	}
	return DefaultPreCompactThresholds()
}

// SudoConfig overrides the hardcoded sudo defaults globally. Each
// field is optional: an empty/unset value preserves the agentd
// fallback. Use Overrides to scope overrides to a specific conv /
// title.
//
// Blocklist is a pointer-to-slice so we can distinguish "field
// absent → keep the default block of permissions.grant /
// permissions.revoke" from "field present, value [] → explicitly
// empty blocklist (you really know what you're doing)". Replace
// semantics, not merge — when set, this field is the complete list.
type SudoConfig struct {
	MaxDuration     string                         `json:"max_duration,omitempty"`
	DefaultDuration string                         `json:"default_duration,omitempty"`
	PopupTimeout    string                         `json:"popup_timeout,omitempty"`
	Blocklist       *[]string                      `json:"blocklist,omitempty"`
	Overrides       map[string]*SudoConfigOverride `json:"overrides,omitempty"`
}

// SudoConfigOverride is the per-conv twin of SudoConfig — same fields
// minus Overrides (no recursion). A non-empty override field replaces
// the corresponding global value; unset fields fall through to the
// global SudoConfig (and then to the agentd hardcoded defaults).
type SudoConfigOverride struct {
	MaxDuration     string    `json:"max_duration,omitempty"`
	DefaultDuration string    `json:"default_duration,omitempty"`
	PopupTimeout    string    `json:"popup_timeout,omitempty"`
	Blocklist       *[]string `json:"blocklist,omitempty"`
}

// agentIDSelectorPrefix tags a sudo-override key as an explicit stable
// agent_id selector, mirroring db.AgentIDPrefix and the `agt_` form
// ResolveSelector accepts. Duplicated as a literal here so the config
// package (hand-edited, low-level) stays free of a db dependency.
const agentIDSelectorPrefix = "agt_"

// MatchSudoOverride picks the SudoConfigOverride that applies to the
// caller (convID / agentID / title). Keys are selector-shaped: a key
// matches if it equals one of the identifiers OR is a prefix of conv-id
// (≥8 chars), of the stable agent_id (an `agt_`-tagged key, ≥12 chars =
// agt_ + 8 hex, the displayed short form), or of the title. The
// agent_id form survives conv rotation where a conv-id key would go
// stale; agentID may be "" when the caller resolved to no actor, which
// simply skips the agent-id branch. The longest matching key wins so a
// more specific override beats a generic prefix. Returns nil when no key
// matches.
func (c *Config) MatchSudoOverride(convID, agentID, title string) *SudoConfigOverride {
	if c == nil || c.Agent == nil || c.Agent.Sudo == nil {
		return nil
	}
	var (
		bestKey string
		best    *SudoConfigOverride
	)
	for k, v := range c.Agent.Sudo.Overrides {
		if !sudoOverrideKeyMatches(k, convID, agentID, title) {
			continue
		}
		if len(k) > len(bestKey) {
			bestKey = k
			best = v
		}
	}
	return best
}

func sudoOverrideKeyMatches(key, convID, agentID, title string) bool {
	if key == "" {
		return false
	}
	if key == convID || key == agentID || key == title {
		return true
	}
	// Stable agent_id selector: an `agt_`-tagged key matches the caller's
	// resolved agent_id by prefix (≥12 chars = agt_ + 8 hex, the displayed
	// short form). Checked before the conv/title prefixes since the tag is
	// an explicit "this is an agent id"; rotation-immune.
	if agentID != "" && strings.HasPrefix(key, agentIDSelectorPrefix) &&
		len(key) >= 12 && len(key) <= len(agentID) && agentID[:len(key)] == key {
		return true
	}
	// Conv-id prefix match: 8 chars is the same threshold ResolveSelector
	// uses for prefix lookups, so config keys can use a stable short form.
	if len(key) >= 8 && convID != "" && len(key) <= len(convID) && convID[:len(key)] == key {
		return true
	}
	if title != "" && len(key) <= len(title) && title[:len(key)] == key {
		return true
	}
	return false
}

// HasDefaultPermission reports whether perm is in the global defaults
// list. Per-agent overrides live in SQLite and are checked separately
// by the daemon's requirePermission — this method only covers the
// defaults half of that lookup.
func (c *Config) HasDefaultPermission(perm string) bool {
	if c == nil || c.Agent == nil {
		return false
	}
	return slices.Contains(c.Agent.DefaultPermissions, perm)
}

// SpawnNameNormalizeEnabled reports whether the spawn surfaces should
// auto-normalize an invalid agent name (agent.NormalizeSpawnName) rather
// than reject it. nil config / absent agent block / absent key all mean ON
// — the out-of-box default, so any typed name "just works"; only an
// explicit "spawn_name_normalize": false disables it. Nil-safe so callers
// need no guard.
func (c *Config) SpawnNameNormalizeEnabled() bool {
	if c == nil || c.Agent == nil || c.Agent.SpawnNameNormalize == nil {
		return true
	}
	return *c.Agent.SpawnNameNormalize
}

// SpawnLabelFromNameEnabled reports whether a spawned agent's session label
// should be derived from its name rather than minted as a random token
// (agent.spawn_label_from_name). Default OFF — nil config / absent agent
// block / absent key all keep the historical "spwn-XXXXXX" label. Nil-safe
// so callers need no guard.
func (c *Config) SpawnLabelFromNameEnabled() bool {
	if c == nil || c.Agent == nil {
		return false
	}
	return c.Agent.SpawnLabelFromName
}

// StatusSnapshotConfig controls the shared runtime gathering window for every consumer.
type StatusSnapshotConfig struct {
	Disabled    bool `json:"disabled,omitempty"`
	FreshnessMS int  `json:"freshness_ms,omitempty"`
}

// FederationTeleportConfig is local policy. Freeze every instance to freeze a
// disconnected fleet; this switch does not claim distributed consensus.
type FederationTeleportConfig struct {
	Disabled bool                 `json:"disabled,omitempty"`
	Limits   TeleportLimits       `json:"limits,omitempty"`
	Backup   TeleportBackupConfig `json:"backup,omitempty"`
}
type TeleportLimits struct {
	Hour           int  `json:"per_hour,omitempty"`
	Day            int  `json:"per_day,omitempty"`
	Chain          int  `json:"per_chain,omitempty"`
	RevisitMinutes int  `json:"revisit_minutes,omitempty"`
	AllowReturn    bool `json:"allow_return,omitempty"`
}

func (l TeleportLimits) Effective() TeleportLimits {
	if l.Hour == 0 {
		l.Hour = 4
	}
	if l.Day == 0 {
		l.Day = 12
	}
	if l.Chain == 0 {
		l.Chain = 16
	}
	if l.RevisitMinutes == 0 {
		l.RevisitMinutes = 10
	}
	return l
}

// ModelProxyPolicy explicitly opts a named HTTP service into an Anthropic
// Messages or OpenAI Responses gateway. Zero budgets never mean unlimited: all six daily limits
// and both token bounds must be positive. Incomplete requests retain their
// conservative input+output reservation until the UTC day rolls over.
type ModelProxyPolicy struct {
	Dialect              string   `json:"dialect,omitempty"`
	LeaseIdleHours       int      `json:"lease_idle_hours,omitempty"`
	PrecountInput        bool     `json:"precount_input,omitempty"`
	BlockedPeers         []string `json:"blocked_peers,omitempty"`
	Enabled              bool     `json:"enabled"`
	Models               []string `json:"models"`
	DailyRequests        int64    `json:"daily_requests"`
	DailyTokens          int64    `json:"daily_tokens"`
	PeerDailyRequests    int64    `json:"peer_daily_requests"`
	PeerDailyTokens      int64    `json:"peer_daily_tokens"`
	SessionDailyRequests int64    `json:"session_daily_requests"`
	SessionDailyTokens   int64    `json:"session_daily_tokens"`
	MaxInputTokens       int64    `json:"max_input_tokens"`
	MaxOutputTokens      int64    `json:"max_output_tokens"`
	MaxConcurrent        int      `json:"max_concurrent"`
	RequestsPerMinute    int      `json:"requests_per_minute"`
	// Byte bounds are independent safety limits, never substitutes for tokens.
	MaxRequestBytes    int64 `json:"max_request_bytes,omitempty"`
	MaxResponseBytes   int64 `json:"max_response_bytes,omitempty"`
	MaxEventBytes      int   `json:"max_event_bytes,omitempty"`
	MaxDurationSeconds int   `json:"max_duration_seconds,omitempty"`
	// Prices adds or overrides the gateway's model prices (USD per million
	// tokens), keyed by model ID or a trailing-* prefix. Usage sharing
	// prices requests with them; built-in Claude and OpenAI API rates apply
	// otherwise.
	Prices map[string]ModelProxyPrice `json:"prices,omitempty"`
}

// ModelProxyPrice is one model's rates in USD per million tokens. A zero
// cache rate falls back to the input rate.
type ModelProxyPrice struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read,omitempty"`
	CacheWrite   float64 `json:"cache_write,omitempty"`
	CacheWrite1h float64 `json:"cache_write_1h,omitempty"`
}

// TeleportBackupConfig deliberately prefers remote availability during an
// origin outage. Epoch reconciliation resolves overlap after a partition.
type TeleportBackupConfig struct {
	RenewSeconds int    `json:"renew_seconds,omitempty"`
	LeaseSeconds int    `json:"lease_seconds,omitempty"`
	GraceSeconds int    `json:"grace_seconds,omitempty"`
	DormantMax   int    `json:"dormant_max,omitempty"`
	Recovery     string `json:"recovery,omitempty"`
	Superseded   string `json:"superseded,omitempty"`
}

func (c TeleportBackupConfig) Effective() TeleportBackupConfig {
	if c.RenewSeconds == 0 {
		c.RenewSeconds = 30
	}
	if c.LeaseSeconds == 0 {
		c.LeaseSeconds = 300
	}
	if c.GraceSeconds == 0 {
		c.GraceSeconds = 120
	}
	if c.DormantMax == 0 {
		c.DormantMax = 4
	}
	if c.Recovery == "" {
		c.Recovery = "auto"
	}
	if c.Superseded == "" {
		c.Superseded = "stop"
	}
	return c
}
func (c TeleportBackupConfig) Validate() error {
	c = c.Effective()
	if c.RenewSeconds < 1 || c.RenewSeconds > 43200 || c.LeaseSeconds < c.RenewSeconds*2 || c.LeaseSeconds > 86400 || c.GraceSeconds < 1 || c.GraceSeconds > 86400 || c.DormantMax < 1 || c.DormantMax > 10000 || c.Recovery != "auto" && c.Recovery != "manual" || c.Superseded != "stop" && c.Superseded != "clone" {
		return fmt.Errorf("invalid teleport backup policy")
	}
	return nil
}
