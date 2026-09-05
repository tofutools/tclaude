// Package config provides configuration loading for tclaude.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/common"
)

// Config represents the tclaude configuration file structure.
type Config struct {
	Notifications   *NotificationConfig    `json:"notifications,omitempty"`
	PreCompactGuard *PreCompactGuardConfig `json:"pre_compact_guard,omitempty"`
	LogLevel        string                 `json:"log_level,omitempty"`
	RecordHooks     bool                   `json:"record_hooks,omitempty"`
	RateLimit       *RateLimitConfig       `json:"ratelimit,omitempty"`
	Agent           *AgentConfig           `json:"agent,omitempty"`

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

	// Audit holds the audit-log retention policy (JOH-268). Absent block /
	// absent keys fall back to the built-in default — see
	// ResolvedAuditRetentionDays.
	Audit *AuditConfig `json:"audit,omitempty"`

	// RemoteAccess configures the optional network-exposed dashboard
	// listener — see RemoteAccessConfig. Absent / disabled (the default)
	// keeps agentd loopback-only.
	RemoteAccess *RemoteAccessConfig `json:"remote_access,omitempty"`

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
	// ShowDebugTab surfaces the dashboard's Debug tab — the daemon
	// self-diagnostics view (poll-timing distributions from /api/perf,
	// TCL-376). Default false: it is a maintainer/troubleshooting surface,
	// so the nav stays uncluttered for everyone not chasing a slow poll.
	// The gate is DISPLAY-only — the daemon records poll timings (and
	// serves /api/perf) either way, so history exists from before the tab
	// was switched on. The dashboard reads the resolved value off the
	// snapshot each poll and toggles body.hide-debug. See
	// (*Config).ShowDebugTab.
	ShowDebugTab bool `json:"show_debug_tab,omitempty"`
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

// CostConfig holds display-only cost knobs.
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
type CostConfig struct {
	EstimateFactor     *float64 `json:"estimate_factor,omitempty"`
	ShowOnSubscription bool     `json:"show_on_subscription,omitempty"`
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
	if f <= 0 {
		return defaultCostFactor
	}
	if f > maxCostEstimateFactor {
		return maxCostEstimateFactor
	}
	return f
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

// ShowDebugTab reports whether the dashboard's Debug tab (daemon
// self-diagnostics — poll-timing distributions, TCL-376) should be shown —
// config dashboard.show_debug_tab. Default false: it is a troubleshooting
// surface, hidden until explicitly enabled. Display-only — the timing
// recorder and /api/perf run regardless. Nil-safe on the receiver.
func (c *Config) ShowDebugTab() bool {
	return c != nil && c.Dashboard != nil && c.Dashboard.ShowDebugTab
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
}

// AWBProxyConfig is the operator's policy for the daemon-mediated AWB proxy —
// Agent Work Board, an agent-first issue tracker with an HTTP API.
//
// It answers the same question LinearProxyConfig does, in AWB's vocabulary:
// which WORKSPACES may agentd reach with the operator's AWB account, on behalf of
// an agent that holds no credentials of its own?
//
// AWB has no filesystem artifact that could anchor an agent the way a git work
// tree anchors the git proxy. A workspace allow-list is therefore the whole scope
// gate, which is why it is fail-closed. This block is the OPERATOR-GLOBAL half;
// the per-agent half is an `awb_workspace` scope on the agent's own
// proxy.awb.read / proxy.awb.write grant (`tclaude agent permissions grant
// <agent> proxy.awb.read --scope awb_workspace=awb`). The two are enforced
// together and neither can widen the other.
//
// Unlike Linear there is no multi-key routing here: one URL is one server, and
// one account reaches every workspace it is a member of. An operator running two
// AWB servers points the daemon at one of them.
type AWBProxyConfig struct {
	// URL is the base URL of the AWB server's HTTP API — "https://awb.example"
	// or "https://example/awb" when a reverse proxy serves it under a path.
	// The proxy joins "/api/…" onto it.
	//
	// It is what OPTS THE OPERATOR IN: with no URL the daemon has no server to
	// call, so `tclaude proxy awb` is not registered and its permission slugs
	// are hidden. Only http:// and https:// are accepted, and a URL carrying
	// userinfo is refused — credentials belong in Username and PasswordFile,
	// where they do not travel in log lines.
	URL string `json:"url,omitempty"`

	// Username is the AWB account every proxied call authenticates as, and
	// therefore the identity AWB attributes every write to. It is also what
	// `claim` records as the assignee when the caller gives no --as, and what
	// `--mine` filters on.
	//
	// Empty means the server holds no users and authenticates nothing, which
	// AWB supports; the daemon then sends no credentials. That is only sensible
	// for a server nothing but the daemon can reach.
	Username string `json:"username,omitempty"`

	// PasswordFile is a file whose contents become the AWB password. When empty
	// the daemon falls back to AWB_PASSWORD in its own environment; with
	// neither, and a Username set, the proxy refuses rather than sending a
	// half-formed credential.
	//
	// The password is deliberately NOT a field of this struct's own JSON: this
	// file is plaintext, shows up in the dashboard's Config tab, and is the
	// sort of thing an operator copies into a bug report. Same treatment as
	// LinearProxyConfig.APIKeyFile — `~/…` is expanded against the home
	// directory of the account agentd runs as, and shell variables are not.
	PasswordFile string `json:"password_file,omitempty"`

	// AllowedWorkspaces is the allow-list of AWB workspace keys the proxy may act
	// on — the prefix of an issue ID, so "awb" authorizes awb-a3f9c1.
	// Compared case-insensitively.
	//
	// EMPTY OR ABSENT DISABLES UNSCOPED GRANTS, exactly as an empty
	// AllowedTeams disables unscoped Linear-proxy grants: an agent whose grant
	// carries an `awb_workspace` scope supplies its own workspaces instead, and
	// when both exist a request must satisfy both. There is deliberately no
	// wildcard: workspace keys are a flat namespace with no hierarchy to match a
	// prefix against, so a wildcard would only ever mean "all of them".
	AllowedWorkspaces []string `json:"allowed_workspaces,omitempty"`
	// LegacyAllowedProjects reads and preserves configs written before AWB
	// renamed projects to workspaces. Effective policy merges it into
	// AllowedWorkspaces; remove after one compatibility cycle.
	LegacyAllowedProjects []string `json:"allowed_projects,omitempty"`

	// AllowWrite permits the mutating verbs (create, update, claim, release,
	// close, reopen, delete, label, dep add/rm, attach add/delete) at all.
	// Default off, so an operator who wants an agent to READ its ticket does
	// not silently also let it write to the tracker under their account. The
	// `proxy.awb.write` permission slug still gates the caller on top of this:
	// the config is the operator's ceiling, the slug is the per-agent grant.
	AllowWrite bool `json:"allow_write,omitempty"`
}

// LinearProxyConfig is the operator's policy for the daemon-mediated Linear
// proxy. It answers the same question GitProxyConfig does, in Linear's
// vocabulary: which teams may agentd reach with the operator's API key, on
// behalf of an agent that holds no key of its own?
//
// Linear has no filesystem artifact that could anchor an agent the way a git
// work tree anchors the git proxy — no `.git/config` equivalent ties a
// conversation to an issue. A team allow-list is therefore the whole scope
// gate, which is why it is mandatory and fail-closed.
//
// This block is the OPERATOR-GLOBAL half of that gate. The per-agent half is a
// `linear_team` scope on the agent's own proxy.linear.read / proxy.linear.write grant
// (`tclaude agent permissions grant <agent> proxy.linear.read --scope
// linear_team=TCL`), which narrows what that one agent may reach. The two are
// enforced together and neither can widen the other.
type LinearProxyConfig struct {
	// AllowedTeams is the allow-list of Linear team keys the proxy may act
	// on — the short prefix in an issue identifier, so "TCL" authorizes
	// TCL-1, TCL-568 and so on. Compared case-insensitively.
	//
	// EMPTY OR ABSENT DISABLES UNSCOPED GRANTS, exactly as an empty
	// AllowedRemotes disables unscoped git-proxy grants: an agent whose grant
	// carries a `linear_team` scope supplies its own teams instead, and when
	// both exist a request must satisfy both. There is deliberately no
	// wildcard and no "allow every team" setting: team keys are a flat
	// namespace with no hierarchy to match a prefix against, so a wildcard
	// would only ever mean "all of them" — which is the setting an operator
	// should have to write out team by team.
	AllowedTeams []string `json:"allowed_teams,omitempty"`

	// APIKeyFile is a file whose contents become the Linear personal API
	// key. When empty the daemon falls back to LINEAR_API_KEY in its own
	// environment; with neither, the proxy reports itself unconfigured and
	// refuses. The key is never written to config.json itself — that file is
	// plaintext and shows up in the dashboard's Config tab and in backups.
	//
	// Accepts `~/…`, expanded against the home directory of the account
	// agentd runs as. Shell variables are NOT expanded — a config file is
	// not a shell.
	//
	// This is the DEFAULT key: it is used for every allowed team that no
	// Workspaces entry claims. An operator whose teams all live in one Linear
	// workspace needs nothing else.
	APIKeyFile string `json:"api_key_file,omitempty"`

	// Workspaces routes named teams to a DIFFERENT key from APIKeyFile.
	//
	// It exists because a Linear personal API key is scoped to ONE workspace:
	// the one its creator was logged into. Within that workspace a single key
	// reaches every team its account can see, whoever created them — so teams
	// that merely have different owners need no entry here. Teams in a
	// SEPARATE workspace cannot be reached by that key at all, at which point
	// the only way to cover them is a second key created in that workspace.
	//
	// Authorization is NOT what this list does. AllowedTeams above and the
	// caller's grant scope decide which teams may be reached; an entry here
	// only says which credential reaches one. A team named here and absent
	// from AllowedTeams stays unreachable, and adding a workspace can never
	// widen what an agent may touch.
	Workspaces []LinearWorkspaceConfig `json:"workspaces,omitempty"`

	// AllowWrite permits the mutating verbs (`issue create`, `issue
	// comment`, `issue update`, `issue link`) at all. Default off, so an
	// operator who wants an agent to READ its ticket does not silently also
	// let it write to the workspace under their name. The `proxy.linear.write`
	// permission slug still gates the caller on top of this: the config is
	// the operator's ceiling, the slug is the per-agent grant.
	AllowWrite bool `json:"allow_write,omitempty"`
}

// LinearWorkspaceConfig binds one Linear workspace's key to the teams that
// live in it — one entry per EXTRA workspace beyond the one
// LinearProxyConfig.APIKeyFile belongs to.
//
// Both fields are required, and a team key may appear in at most one entry. The
// daemon refuses to serve a policy that breaks either rule rather than picking
// a key for an ambiguously-routed team: a request answered by the wrong
// workspace's key does not fail cleanly, it reports the issue as missing.
//
// Note that Linear only guarantees a team key to be unique WITHIN a workspace,
// so two workspaces can each have an "OPS". Nothing in this proxy can tell
// those apart — the allow-list, the `linear_team` grant scope and the issue
// identifier all carry the bare key — so colliding keys across workspaces are
// unsupported rather than merely unrouted.
type LinearWorkspaceConfig struct {
	// Name labels the workspace in diagnostics — `whoami`'s per-workspace
	// breakdown, and the refusal an unreadable key produces. Free text, and
	// only that: nothing routes on it. Blank falls back to the entry's
	// position ("workspace 2").
	Name string `json:"name,omitempty"`

	// APIKeyFile is a file holding the personal API key created IN THIS
	// WORKSPACE. Same handling as LinearProxyConfig.APIKeyFile: `~/…` is
	// expanded, shell variables are not, and the key never enters
	// config.json. Unlike that field there is no environment fallback — one
	// LINEAR_API_KEY cannot stand in for several workspaces, so an entry
	// without a file names no key at all and is refused.
	APIKeyFile string `json:"api_key_file"`

	// Teams is the team keys this workspace's key reaches, compared
	// case-insensitively. Empty makes the entry unreachable, which is a
	// misconfiguration rather than a no-op, so it is refused too.
	Teams []string `json:"teams"`
}

// GitProxyConfig is the operator's policy for the daemon-mediated Git-remote
// and GitHub proxy. It answers one question: which remotes may agentd reach
// with the operator's credentials, on behalf of an agent that cannot reach
// them itself?
//
// It deliberately does NOT decide WHERE an operation may run. That is the git
// work tree containing the agent's own daemon-recorded physical launch
// directory, and it is not operator-tunable — see agentd.resolveProxyRepo for
// exactly what is checked. The proxy lends credentials, never filesystem reach.
type GitProxyConfig struct {
	// AllowedRemotes is the allow-list of remotes the proxy may talk to,
	// written as slash-separated `host/owner/repo` patterns matched
	// case-insensitively against the remote's resolved URL:
	//
	//   "github.com"                  every repo on github.com
	//   "github.com/tofutools"        every repo in that owner
	//   "github.com/tofutools/*"      the same, spelled explicitly
	//   "github.com/tofutools/myrepo" exactly one repo
	//
	// A `*` matches exactly one segment. A pattern with fewer segments than
	// the target matches as a prefix, so "github.com" covers everything on
	// that host.
	//
	// EMPTY OR ABSENT disables unscoped proxy grants. A grant carrying the
	// remote scope can authorize its own remote patterns instead; when both
	// mechanisms are present they are enforced together.
	AllowedRemotes []string `json:"allowed_remotes,omitempty"`

	// ProtectedRefs are branch names the proxy refuses to push to at all,
	// force or not. Absent uses DefaultGitProxyProtectedRefs ("main",
	// "master"); an explicit empty list turns the protection off. Entries
	// are branch-name patterns where `*` matches within one segment, so
	// "release/*" protects a whole namespace.
	//
	// This is a blunt, deliberately non-negotiable guard: the point of the
	// proxy is to let an agent do ordinary feature work, and an agent
	// pushing straight onto the trunk is the failure mode with the worst
	// blast radius.
	ProtectedRefs *[]string `json:"protected_refs,omitempty"`

	// AllowForcePush permits `--force-with-lease` on non-protected refs.
	// Default off. Protected refs are never force-pushable regardless.
	AllowForcePush bool `json:"allow_force_push,omitempty"`

	// SSHKey optionally pins a single private key for the proxy's SSH
	// transport (rendered as `ssh -i <key> -o IdentitiesOnly=yes`). Empty
	// uses the daemon's ambient SSH setup — normally an ssh-agent, which is
	// the preferred posture because no secret then enters the child process
	// at all. The path is the KEY, not a passphrase; the proxy always runs
	// ssh with BatchMode=yes, so a passphrase-protected key that is not
	// already loaded into an agent fails fast instead of hanging the daemon
	// on a prompt.
	SSHKey string `json:"ssh_key,omitempty"`

	// GitHubTokenFile optionally names a file whose contents are the GitHub
	// token handed to `gh` as GH_TOKEN. Empty lets `gh` use the daemon's own
	// authenticated configuration. The token is passed through the child's
	// environment or this file — never through argv, which is world-readable
	// for the process lifetime via /proc/<pid>/cmdline.
	GitHubTokenFile string `json:"github_token_file,omitempty"`
}

// DefaultGitProxyProtectedRefs are the branches the Git proxy refuses to push
// to when the operator has not said otherwise.
var DefaultGitProxyProtectedRefs = []string{"main", "master"}

// ResolvedGitProxy returns the effective Git-proxy policy: the configured
// block with absent fields filled in, and every pattern trimmed and
// lower-cased. It is nil-safe — a nil config, absent agent block, or absent
// git_proxy block all yield a zero policy whose empty AllowedRemotes means
// "proxy disabled".
func (c *Config) ResolvedGitProxy() GitProxyConfig {
	var out GitProxyConfig
	if c != nil && c.Agent != nil && c.Agent.GitProxy != nil {
		src := c.Agent.GitProxy
		out.AllowedRemotes = normalizeGitProxyPatterns(src.AllowedRemotes)
		out.AllowForcePush = src.AllowForcePush
		out.SSHKey = strings.TrimSpace(src.SSHKey)
		out.GitHubTokenFile = strings.TrimSpace(src.GitHubTokenFile)
		if src.ProtectedRefs != nil {
			// An explicit list — including an explicit empty one, which
			// deliberately turns the protection off — wins over the default.
			refs := normalizeGitProxyPatterns(*src.ProtectedRefs)
			out.ProtectedRefs = &refs
			return out
		}
	}
	refs := append([]string(nil), DefaultGitProxyProtectedRefs...)
	out.ProtectedRefs = &refs
	return out
}

// GitProxyEnabled reports whether the operator has opted into the proxy at
// all. Everything else about the feature is gated behind this, so a daemon
// with no git_proxy block never runs `git` or `gh` on an agent's behalf.
func (c *Config) GitProxyEnabled() bool {
	return len(c.ResolvedGitProxy().AllowedRemotes) > 0
}

// ResolvedLinearProxy returns the effective Linear-proxy policy. Nil-safe in
// the same way ResolvedGitProxy is: a nil config, absent agent block, or
// absent linear_proxy block all yield a zero policy whose empty AllowedTeams
// means "proxy disabled".
//
// Team keys go through normalizeGitProxyPatterns despite the name: it trims,
// de-blanks, de-duplicates and lower-cases, which is exactly the treatment a
// team key needs, and the matcher lower-cases the other side too.
func (c *Config) ResolvedLinearProxy() LinearProxyConfig {
	var out LinearProxyConfig
	if c != nil && c.Agent != nil && c.Agent.LinearProxy != nil {
		src := c.Agent.LinearProxy
		out.AllowedTeams = normalizeGitProxyPatterns(src.AllowedTeams)
		out.APIKeyFile = strings.TrimSpace(src.APIKeyFile)
		out.AllowWrite = src.AllowWrite
		out.Workspaces = normalizeLinearWorkspaces(src.Workspaces)
	}
	return out
}

// normalizeLinearWorkspaces trims each workspace entry and normalizes its team
// list the way AllowedTeams is normalized, so the two are directly comparable.
//
// It deliberately does NOT drop malformed entries. An entry with no key file or
// no teams is a mistake the operator has to see: silently discarding it would
// route that workspace's teams to the default key, which is exactly the wrong
// key, and the resulting "issue does not exist" would send them looking at
// Linear rather than at their config. The daemon refuses such a policy — see
// agentd.linearRoutes.
func normalizeLinearWorkspaces(in []LinearWorkspaceConfig) []LinearWorkspaceConfig {
	if len(in) == 0 {
		return nil
	}
	out := make([]LinearWorkspaceConfig, 0, len(in))
	for _, ws := range in {
		out = append(out, LinearWorkspaceConfig{
			Name:       strings.TrimSpace(ws.Name),
			APIKeyFile: strings.TrimSpace(ws.APIKeyFile),
			Teams:      normalizeGitProxyPatterns(ws.Teams),
		})
	}
	return out
}

// LinearProxyEnabled reports whether the operator has opted into the Linear
// proxy OPERATOR-GLOBALLY. It answers a question about this config block alone,
// so it is not the whole enablement rule: an agent whose grant carries a
// `linear_team` scope is reachable with no allowed_teams list at all. The
// daemon's own gate is agentd.linearEffectiveTeams, which consults both.
func (c *Config) LinearProxyEnabled() bool {
	return len(c.ResolvedLinearProxy().AllowedTeams) > 0
}

// LinearProxyConfigured reports whether the operator has set up the Linear
// proxy AT ALL — any allow-list, any key file, any workspace route.
//
// It is deliberately broader than LinearProxyEnabled, and answers a different
// question. LinearProxyEnabled is about the operator's global TEAM POLICY, one
// half of an authorization decision. This is about whether the feature exists
// on this host, which is what the permission catalog needs: a slug hidden from
// the catalog is one an operator cannot grant, so the test for showing it has
// to be "could this ever work here" rather than "is it fully configured".
//
// A key supplied only through LINEAR_API_KEY in the daemon's environment is
// invisible from here; the caller adds that signal, since reading the daemon's
// own environment is not config's job.
func (c *Config) LinearProxyConfigured() bool {
	p := c.ResolvedLinearProxy()
	return len(p.AllowedTeams) > 0 || p.APIKeyFile != "" || len(p.Workspaces) > 0
}

// LinearTeamAllowed reports whether key names a team the operator allow-listed.
// Exact, case-insensitive match on the whole key — there is no prefix or
// wildcard rule here, unlike the remote matcher, because team keys are a flat
// namespace: a prefix match would let "TCL" authorize "TCLX". The `linear_team`
// permission-scope matcher deliberately uses the same rule.
//
// This is the operator half of the gate only. A request is authorized by
// agentd's effective team set, which also folds in the caller's grant scope.
func (p LinearProxyConfig) LinearTeamAllowed(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return false
	}
	return slices.Contains(p.AllowedTeams, key)
}

// ResolvedAWBProxy returns the effective AWB-proxy policy. Nil-safe in the same
// way ResolvedLinearProxy is: a nil config, absent agent block, or absent
// awb_proxy block all yield a zero policy whose empty URL means "proxy off".
//
// Workspace keys go through normalizeGitProxyPatterns despite the name: it trims,
// de-blanks, de-duplicates and lower-cases, which is exactly the treatment an
// AWB workspace key needs, and the matcher lower-cases the other side too.
func (c *Config) ResolvedAWBProxy() AWBProxyConfig {
	var out AWBProxyConfig
	if c != nil && c.Agent != nil && c.Agent.AWBProxy != nil {
		src := c.Agent.AWBProxy
		out.URL = strings.TrimRight(strings.TrimSpace(src.URL), "/")
		out.Username = strings.TrimSpace(src.Username)
		out.PasswordFile = strings.TrimSpace(src.PasswordFile)
		allowed := append(append([]string{}, src.AllowedWorkspaces...), src.LegacyAllowedProjects...)
		out.AllowedWorkspaces = normalizeGitProxyPatterns(allowed)
		out.AllowWrite = src.AllowWrite
	}
	return out
}

// AWBProxyEnabled reports whether the operator has pointed the daemon at an AWB
// server at all. It is the registration gate — the question "does this host
// have an AWB proxy" — and deliberately NOT the authorization gate: which
// workspaces a caller may reach is agentd.awbEffectiveWorkspaces, which folds the
// allow-list below together with the caller's own grant scope.
//
// It keys on the URL rather than on AllowedWorkspaces because a scope-only
// posture (no operator list, per-agent `awb_workspace` grants) is a supported
// configuration, and an operator running it still has a proxy.
func (c *Config) AWBProxyEnabled() bool {
	return c.ResolvedAWBProxy().URL != ""
}

// AWBWorkspaceAllowed reports whether key names a workspace the operator
// allow-listed. Exact, case-insensitive match on the whole key — there is no
// prefix or wildcard rule here, for the same reason LinearTeamAllowed has none:
// workspace keys are a flat namespace, and a prefix match would let "web"
// authorize "webhooks".
//
// This is the operator half of the gate only. A request is authorized by
// agentd's effective workspace set, which also folds in the caller's grant scope.
func (p AWBProxyConfig) AWBWorkspaceAllowed(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return false
	}
	return slices.Contains(p.AllowedWorkspaces, key)
}

// normalizeGitProxyPatterns trims, lower-cases and de-blanks a pattern list,
// preserving order and dropping duplicates. Lower-casing is safe here because
// every pattern names a DNS host, a forge owner/repo, or a branch — and both
// the remote matcher and the ref matcher compare lower-cased.
func normalizeGitProxyPatterns(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		p := strings.ToLower(strings.Trim(strings.TrimSpace(raw), "/"))
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
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

// NotificationConfig holds settings for OS notifications.
type NotificationConfig struct {
	Enabled             bool             `json:"enabled"`
	Transitions         []TransitionRule `json:"transitions,omitempty"`
	CooldownSeconds     int              `json:"cooldown_seconds,omitempty"`
	NotificationCommand []string         `json:"notification_command,omitempty"`

	// HumanMessages controls whether a `tclaude agent notify-human`
	// message also raises an OS notification (the desktop companion to
	// the dashboard Messages tab). It is a *bool so the unset/zero state
	// is distinguishable from an explicit false: within an enabled
	// notification block it defaults ON — the human asked notify-human to
	// also ping the desktop — and is silenced only by an explicit
	// "human_messages": false. See NotifyHumanMessages.
	HumanMessages *bool `json:"human_messages,omitempty"`

	// Delivery selects WHERE an already-decided notification is raised.
	// Every gate above it (enabled, transitions, per-agent/group filters,
	// cooldown) is unchanged — this only picks the output channel:
	//
	//	"os"      — the platform notifier (D-Bus / toast / terminal-notifier),
	//	            or notification_command when set. The historical
	//	            behaviour and the default for an empty value.
	//	"browser" — the notification is queued for the agentd dashboard,
	//	            which raises a Web Notification from the browser. Works
	//	            when the human is remote (the dashboard is reachable but
	//	            the daemon's desktop is not) and when the notifying
	//	            process is sandboxed away from the session D-Bus.
	//	"both"    — raise it in both places.
	//
	// Browser delivery needs a dashboard tab open, granted the browser's
	// notification permission, in a secure context (https or localhost).
	Delivery string `json:"delivery,omitempty"`
}

// Notification delivery channels — the accepted values of
// NotificationConfig.Delivery. NotifyDeliveryOS is also the meaning of
// the empty/unset value.
const (
	NotifyDeliveryOS      = "os"
	NotifyDeliveryBrowser = "browser"
	NotifyDeliveryBoth    = "both"
)

// IsNotifyDelivery reports whether s is an accepted delivery value. The
// empty string is accepted — it is the unset state, which reads as "os".
func IsNotifyDelivery(s string) bool {
	switch s {
	case "", NotifyDeliveryOS, NotifyDeliveryBrowser, NotifyDeliveryBoth:
		return true
	}
	return false
}

// DeliverToOS reports whether notifications should reach the platform
// notifier. Unset/unknown → true, so a config written by a newer tclaude
// (or a typo that slipped past Validate) degrades to the historical
// desktop behaviour rather than to silence.
func (c *NotificationConfig) DeliverToOS() bool {
	if c == nil {
		return true
	}
	return c.Delivery != NotifyDeliveryBrowser
}

// DeliverToBrowser reports whether notifications should also be queued
// for the dashboard to raise as Web Notifications.
func (c *NotificationConfig) DeliverToBrowser() bool {
	if c == nil {
		return false
	}
	return c.Delivery == NotifyDeliveryBrowser || c.Delivery == NotifyDeliveryBoth
}

// NotifyHumanMessages reports whether a notify-human message should also
// raise an OS notification. It requires the master switch (Enabled);
// within that it defaults ON and is suppressed only by an explicit
// "human_messages": false. nil receiver / disabled block → false.
func (c *NotificationConfig) NotifyHumanMessages() bool {
	if c == nil || !c.Enabled {
		return false
	}
	return c.HumanMessages == nil || *c.HumanMessages
}

// RateLimitConfig holds settings for rate limit
type RateLimitConfig struct {
	FiveHourPercentMaxUsed float64 `json:"five_hour_percent_max_used"`
	SevenDayPercentMaxUsed float64 `json:"seven_day_percent_max_used"`
}

// TransitionRule defines a state transition that triggers a notification.
// Use "*" as a wildcard to match any state. A self-transition (from ==
// to, e.g. an idle session re-stamped idle) never notifies, regardless
// of rules — notify.OnStateTransition drops it before matching.
type TransitionRule struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// DefaultConfig returns a config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		LogLevel: "info",
		Notifications: &NotificationConfig{
			Enabled: false,
			Transitions: []TransitionRule{
				{From: "*", To: "idle"},
				{From: "*", To: "awaiting_permission"},
				{From: "*", To: "awaiting_input"},
				{From: "*", To: "error"},
				{From: "*", To: "exited"},
			},
			CooldownSeconds: 5,
		},
		RateLimit: nil,
	}
}

// ConfigDir returns the tclaude root directory (~/.tclaude). It stays the ROOT
// of the layout; private state lives under DataDir() and the agent-reachable
// socket under APIDir(). See pkg/common.TclaudeDir for the rationale.
func ConfigDir() string {
	return common.TclaudeDir()
}

// DataDir returns the private state directory (~/.tclaude/data) that is denied
// to sandboxed agents. It is the home for db.sqlite, operator_token,
// processes/, the logs, and config.json. Delegates to pkg/common so the
// low-level state packages (which cannot import config without a cycle) and
// this package resolve the same path.
func DataDir() string {
	return common.TclaudeDataDir()
}

// APIDir returns the agent-reachable directory (~/.tclaude/api) that holds the
// agentd Unix socket. Unlike DataDir(), it stays reachable from a sandboxed
// agent so coordination (`tclaude agent …`) keeps working.
func APIDir() string {
	return common.TclaudeAPIDir()
}

// ConfigPath returns the path to the config file
// (~/.tclaude/data/config.json — private daemon state, migrated automatically
// from the legacy ~/.tclaude/config.json location by the daemon at startup).
func ConfigPath() string {
	return common.TclaudeStatePath("config.json")
}

// Load loads the config from ~/.tclaude/config.json.
// Returns default config if file doesn't exist.
func Load() (*Config, error) {
	path := ConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		// A managed agent deliberately cannot read ~/.tclaude. Its agent-facing
		// socket is sufficient for permission-gated CLI calls, so treat the
		// inaccessible operator config like an absent config instead of printing
		// a warning before every `tclaude agent` command.
		if os.IsPermission(err) && privateConfigIntentionallyInaccessible() {
			return DefaultConfig(), nil
		}
		slog.Warn("Unable to load config", "err", err)
		return DefaultConfig(), err
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		slog.Warn("Unable to load config", "err", err)
		return DefaultConfig(), err
	}

	Normalize(&config)
	return &config, nil
}

// privateConfigIntentionallyInaccessible reports whether this process is an
// agent whose sandbox deliberately denies ~/.tclaude.
func privateConfigIntentionallyInaccessible() bool {
	return agentipc.ManagedAgentProcess()
}

// NotificationsPresent reports whether the on-disk config file already
// contains a "notifications" block. It probes the raw bytes BEFORE
// Normalize seeds a default block, letting callers (notably `tclaude
// setup`) tell a deliberately-configured state — including one the user
// turned off — apart from a never-configured fresh install. A missing,
// unreadable or unparseable file, or an explicit "notifications": null,
// all report false.
func NotificationsPresent() bool {
	data, err := os.ReadFile(ConfigPath())
	if err != nil {
		return false
	}
	var probe struct {
		Notifications *json.RawMessage `json:"notifications"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	return probe.Notifications != nil
}

// Normalize fills in tclaude's defaults and clamps out-of-range values
// on a Config in place: an empty log level becomes "info", a missing
// notifications block is populated, a zero cooldown / empty transition
// list fall back to defaults, and an out-of-range rate-limit percent is
// clamped to its safe default. It is idempotent.
//
// Load runs it after unmarshalling the config file. The dashboard's
// visual config editor also runs it (after Validate) so the form, the
// diff preview and the bytes written to disk all agree on one canonical
// shape — there is no second "Load re-applies defaults" surprise.
func Normalize(c *Config) {
	if c == nil {
		return
	}
	canonicalizePermissionNames(c)
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.Notifications == nil {
		// No notifications block at all → seed the full defaults
		// (enabled=false, the five default transition rules, cooldown 5).
		c.Notifications = DefaultConfig().Notifications
	} else {
		if c.Notifications.CooldownSeconds == 0 {
			c.Notifications.CooldownSeconds = 5
		}
		// NB: an *existing* notifications block with an empty Transitions
		// list is left empty on purpose — it means "notify on no state
		// transition" (e.g. the per-type checklist with every box
		// unchecked, leaving only human-message notifications). Re-seeding
		// the defaults here would make unchecking the last type silently
		// snap back to all-on. Only an absent block (the nil branch above)
		// gets the default rules.
	}
	if c.RateLimit != nil {
		if v := c.RateLimit.FiveHourPercentMaxUsed; v <= 0 || v > 100 {
			slog.Warn("Invalid ratelimit.five_hour_percent_max_used; using default", "value", v)
			c.RateLimit.FiveHourPercentMaxUsed = 99.0
		}
		if v := c.RateLimit.SevenDayPercentMaxUsed; v <= 0 || v > 100 {
			slog.Warn("Invalid ratelimit.seven_day_percent_max_used; using default", "value", v)
			c.RateLimit.SevenDayPercentMaxUsed = 99.9
		}
	}
}

var semanticProxyPermissionRenames = map[string]string{
	"git.read":     "proxy.git.read",
	"git.push":     "proxy.git.push",
	"github.read":  "proxy.github.read",
	"github.write": "proxy.github.write",
	"linear.read":  "proxy.linear.read",
	"linear.write": "proxy.linear.write",
}

var groupPermissionRenames = map[string]string{
	"groups.rm":                  "groups.delete",
	"groups.stop":                "groups.members.stop",
	"groups.resume":              "groups.members.resume",
	"groups.retire":              "groups.members.retire",
	"groups.spawn":               "groups.members.spawn",
	"groups.own":                 "groups.owners.manage",
	"member.add":                 "groups.members.add",
	"member.remove":              "groups.members.remove",
	"member.redesignate":         "groups.members.update",
	"groups.description":         "groups.settings.description",
	"groups.default-dir":         "groups.settings.default-dir",
	"groups.default-context":     "groups.settings.default-context",
	"groups.default-spawn-group": "groups.settings.default-spawn-target",
	"groups.default-profile":     "groups.settings.default-profile",
	"groups.max-members":         "groups.settings.max-members",
	"groups.notifications":       "groups.settings.notifications",
	"groups.remote-control":      "groups.settings.remote-control-policy",
	"groups.permissions":         "groups.settings.member-permissions",
	"groups.owner-scopes":        "groups.settings.owner-scopes",
	"groups.link.rm":             "groups.link.remove",
}

// canonicalizePermissionNames upgrades every config field that holds
// permission slugs. It also runs from Normalize so a read-only managed process
// gets the correct effective policy even when it cannot rewrite operator state.
func canonicalizePermissionNames(c *Config) bool {
	if c == nil || c.Agent == nil {
		return false
	}
	changed := renamePermissionList(&c.Agent.DefaultPermissions, semanticProxyPermissionRenames, groupPermissionRenames)
	if c.Agent.Sudo == nil {
		return changed
	}
	if renamePermissionList(c.Agent.Sudo.Blocklist, semanticProxyPermissionRenames, groupPermissionRenames) {
		changed = true
	}
	for _, override := range c.Agent.Sudo.Overrides {
		if override != nil && renamePermissionList(override.Blocklist, semanticProxyPermissionRenames, groupPermissionRenames) {
			changed = true
		}
	}
	return changed
}

func renamePermissionList(slugs *[]string, renameSets ...map[string]string) bool {
	if slugs == nil {
		return false
	}
	existing := make(map[string]bool, len(*slugs))
	for _, slug := range *slugs {
		existing[slug] = true
	}
	changed := false
	out := make([]string, 0, len(*slugs))
	for _, slug := range *slugs {
		canonical, legacy := slug, false
		for _, renames := range renameSets {
			if renamed, ok := renames[slug]; ok {
				canonical, legacy = renamed, true
				break
			}
		}
		if !legacy {
			out = append(out, slug)
			continue
		}
		changed = true
		if existing[canonical] {
			continue
		}
		existing[canonical] = true
		out = append(out, canonical)
	}
	if changed {
		*slugs = out
	}
	return changed
}

// MigratePermissionNames persists permission-slug renames in config.json.
// Missing and sandbox-inaccessible config are clean no-ops; malformed config
// is returned to the caller so it can warn without overwriting human edits.
func MigratePermissionNames() error {
	saveMu.Lock()
	defer saveMu.Unlock()
	data, err := os.ReadFile(ConfigPath())
	if err != nil {
		if os.IsNotExist(err) || (os.IsPermission(err) && privateConfigIntentionallyInaccessible()) {
			return nil
		}
		return err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	if !canonicalizePermissionNames(&cfg) {
		return nil
	}
	Normalize(&cfg)
	return saveLocked(&cfg)
}

// saveMu serializes config-file writes within this process. Save's
// atomic rename prevents torn files, but not lost updates: two
// concurrent load→modify→save sequences would silently drop one
// writer's change. Update holds this mutex across the whole
// read-modify-write; Save holds it for the write so a direct Save can
// never land inside an Update's critical section. Cross-process races
// remain possible (any tclaude command may Save) but in practice all
// concurrent writers live in the agentd daemon.
var saveMu sync.Mutex

// Update performs a serialized read-modify-write of the config file:
// load, hand the result (plus any load error) to mutate, then save —
// all under saveMu, so concurrent Updates can't drop each other's
// changes and a plain Save can't interleave. mutate receives the load
// error rather than Update swallowing it, because callers differ on
// how to treat a corrupt file (refuse vs. overwrite); returning a
// non-nil error from mutate aborts without writing and is returned
// as-is, so callers can use sentinel errors to pick a response.
// Returns the saved config on success.
func Update(mutate func(cfg *Config, loadErr error) error) (*Config, error) {
	saveMu.Lock()
	defer saveMu.Unlock()
	cfg, loadErr := Load()
	if err := mutate(cfg, loadErr); err != nil {
		return nil, err
	}
	if err := saveLocked(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save writes the config to ~/.tclaude/config.json atomically: the
// bytes go to a sibling temp file which is then renamed over the
// target. A crash, power loss or disk-full partway through must never
// leave a truncated config.json — the next Load would silently degrade
// to DefaultConfig and revert every persisted setting. Rename within a
// directory is atomic on POSIX and replace-existing on Windows.
//
// For read-modify-write sequences use Update instead — a bare
// Load→Save can drop a concurrent writer's change.
func Save(config *Config) error {
	saveMu.Lock()
	defer saveMu.Unlock()
	return saveLocked(config)
}

func saveLocked(config *Config) error {
	// Capture the compatibility-selected target once: an old daemon can exit
	// during this save, but the whole atomic write must stay in one layout.
	target := ConfigPath()
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "config-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Removes the temp file on every error path; a no-op once the
	// rename below has consumed it.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp makes the file 0600; match the historical 0644.
	if err := os.Chmod(tmpName, 0644); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

// Validate checks a Config for problems that would make it unsafe or
// nonsensical to persist, returning a list of human-readable error
// strings (empty when the config is acceptable). It is the gatekeeper
// for the dashboard's visual config editor: every problem is reported
// at once so the human fixes them in a single pass instead of one
// failed save at a time. Load() is deliberately more lenient — it
// degrades a bad value to a default and carries on — but a human
// editing config through the dashboard wants to be told.
func Validate(c *Config) []string {
	if c == nil {
		return []string{"config is nil"}
	}
	var errs []string

	switch c.LogLevel {
	case "", "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Sprintf("log_level %q is not one of debug, info, warn, error", c.LogLevel))
	}

	if g := c.PreCompactGuard; g != nil {
		// Only validate the explicit ladder; an enabled guard with no
		// thresholds falls back to the built-in defaults, which are
		// known-good. A configured threshold must be a sane (window,
		// floor) pair: positive sizes and a floor that fits inside the
		// window (a floor ≥ window can never be reached, so the guard
		// would block every compaction forever).
		for i, t := range g.Thresholds {
			switch {
			case t.WindowSize <= 0:
				errs = append(errs, fmt.Sprintf("pre_compact_guard.thresholds[%d].window_size must be positive", i))
			case t.MinTokens <= 0:
				errs = append(errs, fmt.Sprintf("pre_compact_guard.thresholds[%d].min_tokens must be positive", i))
			case t.MinTokens >= t.WindowSize:
				errs = append(errs, fmt.Sprintf("pre_compact_guard.thresholds[%d].min_tokens (%d) must be less than window_size (%d)", i, t.MinTokens, t.WindowSize))
			}
		}
	}

	if c.RateLimit != nil {
		if v := c.RateLimit.FiveHourPercentMaxUsed; v <= 0 || v > 100 {
			errs = append(errs, fmt.Sprintf("ratelimit.five_hour_percent_max_used %g is out of range (>0 and ≤100)", v))
		}
		if v := c.RateLimit.SevenDayPercentMaxUsed; v <= 0 || v > 100 {
			errs = append(errs, fmt.Sprintf("ratelimit.seven_day_percent_max_used %g is out of range (>0 and ≤100)", v))
		}
	}

	if oc := c.OpenCode; oc != nil && oc.LegacyLongContextPricingCutoff != nil &&
		*oc.LegacyLongContextPricingCutoff <= 0 {
		errs = append(errs, fmt.Sprintf(
			"opencode.legacy_long_context_pricing_cutoff %d must be positive (default %d)",
			*oc.LegacyLongContextPricingCutoff, DefaultOpenCodeLegacyLongContextPricingCutoff))
	}

	// claude_cleanup_period_days maps to Claude Code's cleanupPeriodDays, which
	// Claude Code requires to be ≥ 1. 0 / absent is our "leave the key alone"
	// sentinel; a negative value is a typo we reject so it never silently
	// becomes a no-op or reaches settings.json.
	if c.ClaudeCleanupPeriodDays < 0 {
		errs = append(errs, fmt.Sprintf("claude_cleanup_period_days %d must not be negative (0 = leave Claude Code's default; a positive day count overrides it)", c.ClaudeCleanupPeriodDays))
	}

	// Only validate the bind when the listener is actually enabled: a
	// half-typed bind left behind while disabled is harmless (nothing starts),
	// so it must not block an unrelated save.
	if r := c.RemoteAccess; r != nil && r.Enabled {
		if r.Bind == "" {
			errs = append(errs, "remote_access.enabled is set but remote_access.bind is empty; set a bind address (e.g. 0.0.0.0:8443)")
		} else if _, port, err := net.SplitHostPort(r.Bind); err != nil {
			// A non-empty bind must be a host:port the listener can actually
			// bind to — net.Listen("tcp", …) needs the port. Catch a missing
			// port here (as a clean save-time error) rather than letting
			// startRemoteServer fail at boot and only log it.
			errs = append(errs, fmt.Sprintf("remote_access.bind %q is not a valid host:port (e.g. 0.0.0.0:8443): %v", r.Bind, err))
		} else if n, perr := strconv.Atoi(port); perr != nil || n < 1 || n > 65535 {
			// Require an explicit numeric port the operator can dial. A named
			// service ("https") or 0 would technically listen — but 0 binds a
			// random OS-assigned port nobody can reach by URL, and a name is a
			// surprise; reject both so the configured port is the one served.
			errs = append(errs, fmt.Sprintf("remote_access.bind %q needs a numeric port 1–65535 (got %q)", r.Bind, port))
		}
	}

	if c.Notifications != nil {
		if c.Notifications.CooldownSeconds < 0 {
			errs = append(errs, "notifications.cooldown_seconds must not be negative")
		}
		if !IsNotifyDelivery(c.Notifications.Delivery) {
			errs = append(errs, fmt.Sprintf("notifications.delivery %q must be one of %q, %q, %q (or absent)",
				c.Notifications.Delivery, NotifyDeliveryOS, NotifyDeliveryBrowser, NotifyDeliveryBoth))
		}
		for i, tr := range c.Notifications.Transitions {
			if tr.From == "" || tr.To == "" {
				errs = append(errs, fmt.Sprintf("notifications.transitions[%d] needs both from and to (use \"*\" for any state)", i))
			}
		}
	}

	if c.Agent != nil {
		a := c.Agent
		if dir := strings.TrimSpace(a.ResourceDelegationDir); dir != "" && !filepath.IsAbs(dir) {
			errs = append(errs, fmt.Sprintf("agent.resource_delegation_dir %q must be an absolute path", dir))
		}
		if a.CloneCooldown != "" {
			if d, err := time.ParseDuration(a.CloneCooldown); err != nil {
				errs = append(errs, fmt.Sprintf("agent.clone_cooldown %q is not a valid duration (e.g. \"1m\", \"30s\", \"0\")", a.CloneCooldown))
			} else if d < 0 {
				errs = append(errs, "agent.clone_cooldown must not be negative")
			}
		}
		if a.SpawnMaxPerHour != nil && *a.SpawnMaxPerHour < 0 {
			errs = append(errs, "agent.spawn_max_per_hour must not be negative (0 = unlimited)")
		}
		if a.DashboardPort < 0 || a.DashboardPort > 65535 {
			errs = append(errs, fmt.Sprintf("agent.dashboard_port %d is out of range (1–65535, or 0/absent for a random free port)", a.DashboardPort))
		}
		if b := strings.TrimSpace(a.DashboardBind); b != "" {
			// A HOST only — the port lives in dashboard_port. Catch the most
			// common mistake (a host:port pasted in here) with a clean save-time
			// message; a bare IPv6 like "::1" / "::" trips SplitHostPort's
			// too-many-colons error and is correctly left alone. Anything else
			// that can't be bound surfaces as a fatal net.Listen error at daemon
			// startup (see agentd.startPopupServer), the same way an in-use
			// dashboard_port does.
			if _, _, err := net.SplitHostPort(b); err == nil {
				errs = append(errs, fmt.Sprintf("agent.dashboard_bind %q must be a host only, without a port (set the port via agent.dashboard_port); e.g. \"127.0.0.1\", \"0.0.0.0\", or \"::\"", b))
			}
		}
		if cn := a.ContextNudge; cn != nil {
			// When the nudge is enabled, 0 is a footgun: Resolved()
			// silently rewrites a non-positive ladder value to its
			// built-in default, so the human's "0" never takes effect.
			// Require a real 1–100 value while enabled; tolerate 0 (the
			// inert zero value) when the nudge is off.
			lo := 0
			if cn.Enabled {
				lo = 1
			}
			if cn.MinPct < lo || cn.MinPct > 100 {
				errs = append(errs, fmt.Sprintf("agent.context_nudge.min_pct %d is out of range (%d–100)", cn.MinPct, lo))
			}
			if cn.IntervalPct < lo || cn.IntervalPct > 100 {
				errs = append(errs, fmt.Sprintf("agent.context_nudge.interval_pct %d is out of range (%d–100)", cn.IntervalPct, lo))
			}
		}
		if rc := a.RetiredCleanup; rc != nil {
			// after_days is a permanent-delete threshold, so 0 while the
			// sweep is enabled is a footgun: ResolvedRetiredCleanup silently
			// rewrites a non-positive window to the ~1-year default, so the
			// human's "0" never takes effect. Require a real ≥1 value while
			// enabled; tolerate 0 (the inert zero value) when it's off.
			lo := 0
			if rc.Enabled {
				lo = 1
			}
			if rc.AfterDays < lo || rc.AfterDays > MaxRetiredCleanupAfterDays {
				errs = append(errs, fmt.Sprintf("agent.retired_cleanup.after_days %d is out of range (must be %d–%d — it is the number of days an agent stays retired before it is permanently deleted)", rc.AfterDays, lo, MaxRetiredCleanupAfterDays))
			}
		}
		errs = append(errs, validateSudo(a.Sudo)...)
	}

	if cc := c.Cost; cc != nil && cc.EstimateFactor != nil {
		if f := *cc.EstimateFactor; f <= 0 || f > maxCostEstimateFactor {
			errs = append(errs, fmt.Sprintf("cost.estimate_factor %g is out of range (>0 and ≤%g) — it is a display multiplier, e.g. 1.1 for +10%%", f, maxCostEstimateFactor))
		}
	}

	// usage.idle_timeout is a Go duration string; reject anything that
	// doesn't parse or isn't positive so the human is told, rather than
	// letting ResolvedUsageIdleTimeout silently fall back to the default.
	if uc := c.Usage; uc != nil && uc.IdleTimeout != "" {
		if d, err := time.ParseDuration(uc.IdleTimeout); err != nil {
			errs = append(errs, fmt.Sprintf("usage.idle_timeout %q is not a valid duration (e.g. %q for 3 days, or %q) — it is how long the last-known usage reading stays on the dashboard after its source goes idle", uc.IdleTimeout, "72h", "30m"))
		} else if d <= 0 {
			errs = append(errs, fmt.Sprintf("usage.idle_timeout %q must be positive — it is how long the last-known usage reading stays on the dashboard after its source goes idle", uc.IdleTimeout))
		}
	}

	// The resume thresholds are minute / token counts handed verbatim to
	// Claude Code, which parses them as non-negative integers; a negative
	// value is meaningless (and CC would reject it), so flag it rather than
	// inject a var CC ignores. 0 is allowed — it FORCES the prompt for every
	// resume, the deliberate inverse of the suppress sentinel.
	if cr := c.ClaudeResume; cr != nil {
		if cr.ThresholdMinutes != nil && *cr.ThresholdMinutes < 0 {
			errs = append(errs, fmt.Sprintf("claude_resume.threshold_minutes %d must not be negative (use a large value to suppress the prompt, 0 to always show it)", *cr.ThresholdMinutes))
		}
		if cr.TokenThreshold != nil && *cr.TokenThreshold < 0 {
			errs = append(errs, fmt.Sprintf("claude_resume.token_threshold %d must not be negative (use a large value to suppress the prompt, 0 to always show it)", *cr.TokenThreshold))
		}
	}

	if s := c.Slop; s != nil {
		if s.MusicVolume != nil && (*s.MusicVolume < 0 || *s.MusicVolume > 100) {
			errs = append(errs, fmt.Sprintf("slop.music_volume %d is out of range (0–100)", *s.MusicVolume))
		}
		if s.EffectsVolume != nil && (*s.EffectsVolume < 0 || *s.EffectsVolume > 100) {
			errs = append(errs, fmt.Sprintf("slop.effects_volume %d is out of range (0–100)", *s.EffectsVolume))
		}
		// An empty/absent channel resolves to the default; only a non-empty
		// value outside the allowlist is an error worth flagging.
		if s.Channel != nil {
			if id := strings.TrimSpace(*s.Channel); id != "" && !IsKnownSlopChannel(id) {
				errs = append(errs, fmt.Sprintf("slop.channel %q is not a known SomaFM channel (one of: %s)",
					*s.Channel, strings.Join(SlopChannels, ", ")))
			}
		}
	}

	if lr := c.LogRotation; lr != nil {
		if lr.MaxSize != "" {
			if _, err := common.ParseSize(lr.MaxSize); err != nil {
				errs = append(errs, fmt.Sprintf("log_rotation.max_size %q is not a valid size (e.g. \"10MiB\", \"50m\", or \"0\" to disable)", lr.MaxSize))
			}
		}
		if lr.Keep < 0 {
			errs = append(errs, fmt.Sprintf("log_rotation.keep %d must not be negative (0 = built-in default)", lr.Keep))
		}
	}

	if f := c.Focus; f != nil && f.Tile != nil {
		t := f.Tile
		// An empty/absent layout resolves to the default (grid); only a
		// non-empty value outside the known set is worth flagging.
		if t.Layout != "" && normalizeTileLayout(t.Layout) == "" {
			errs = append(errs, fmt.Sprintf("focus.tile.layout %q is not one of %s, %s, %s, %s",
				t.Layout, TileLayoutGrid, TileLayoutColumns, TileLayoutRows, TileLayoutCascade))
		}
		if t.Gap != nil && (*t.Gap < 0 || *t.Gap > maxTilePixels) {
			errs = append(errs, fmt.Sprintf("focus.tile.gap %d is out of range (0–%d pixels)", *t.Gap, maxTilePixels))
		}
		if t.Margin != nil && (*t.Margin < 0 || *t.Margin > maxTilePixels) {
			errs = append(errs, fmt.Sprintf("focus.tile.margin %d is out of range (0–%d pixels)", *t.Margin, maxTilePixels))
		}
	}

	if c.Dashboard != nil && c.Dashboard.TerminalAttach != nil {
		t := c.Dashboard.TerminalAttach
		switch t.Mode {
		case "", TerminalAttachResizeRepair, TerminalAttachResizeInitial, TerminalAttachResizePreAttach:
		default:
			errs = append(errs, fmt.Sprintf(
				"dashboard.terminal_attach.mode %q is not one of %s, %s, %s",
				t.Mode, TerminalAttachResizeRepair, TerminalAttachResizeInitial, TerminalAttachResizePreAttach))
		}
		for name, value := range map[string]*int{
			"initial_resize_delay_ms": t.InitialResizeDelayMS,
			"repair_delay_ms":         t.RepairDelayMS,
			"pre_attach_delay_ms":     t.PreAttachDelayMS,
		} {
			if value != nil && (*value < 0 || *value > MaxTerminalAttachDelayMS) {
				errs = append(errs, fmt.Sprintf(
					"dashboard.terminal_attach.%s %d is out of range (0–%d milliseconds)",
					name, *value, MaxTerminalAttachDelayMS))
			}
		}
	}

	// An empty/absent scheme resolves to the default; only a non-empty value
	// outside the known set is worth flagging (mirrors focus.tile.layout).
	if t := c.TUI; t != nil && t.ColorScheme != "" && normalizeTUIColorScheme(t.ColorScheme) == "" {
		errs = append(errs, fmt.Sprintf("tui.color_scheme %q is not one of %s, %s",
			t.ColorScheme, TUIColorSchemeDefault, TUIColorSchemeHighContrast))
	}

	if f := c.Features; f != nil {
		switch f.GroupAttachments {
		case "", GroupAttachmentsOff, GroupAttachmentsFloat, GroupAttachmentsFixed:
		default:
			errs = append(errs, fmt.Sprintf(
				"features.group_attachments %q is not one of %s, %s, %s",
				f.GroupAttachments, GroupAttachmentsOff, GroupAttachmentsFloat, GroupAttachmentsFixed))
		}
	}

	return errs
}

// validateSudo reports duration-parse problems in a SudoConfig and its
// per-conv overrides. Split out of Validate to keep the nesting flat.
func validateSudo(s *SudoConfig) []string {
	if s == nil {
		return nil
	}
	var errs []string
	chk := func(label, val string) {
		if val == "" {
			return
		}
		if d, err := time.ParseDuration(val); err != nil {
			errs = append(errs, fmt.Sprintf("%s %q is not a valid duration (e.g. \"30m\", \"2h\")", label, val))
		} else if d < 0 {
			errs = append(errs, label+" must not be negative")
		}
	}
	chk("agent.sudo.max_duration", s.MaxDuration)
	chk("agent.sudo.default_duration", s.DefaultDuration)
	chk("agent.sudo.popup_timeout", s.PopupTimeout)
	for k, ov := range s.Overrides {
		if ov == nil {
			continue
		}
		chk(fmt.Sprintf("agent.sudo.overrides[%q].max_duration", k), ov.MaxDuration)
		chk(fmt.Sprintf("agent.sudo.overrides[%q].default_duration", k), ov.DefaultDuration)
		chk(fmt.Sprintf("agent.sudo.overrides[%q].popup_timeout", k), ov.PopupTimeout)
	}
	return errs
}

// MatchesTransition checks if a state transition matches any configured rule.
func (c *NotificationConfig) MatchesTransition(from, to string) bool {
	if c == nil || !c.Enabled {
		return false
	}

	for _, rule := range c.Transitions {
		fromMatch := rule.From == "*" || rule.From == from
		toMatch := rule.To == "*" || rule.To == to
		if fromMatch && toMatch {
			return true
		}
	}
	return false
}

// NotifyTypes is the canonical set of destination states the friendly
// per-type notification selector (the top-bar bell popover and the Config
// tab checklist) toggles. Each "type" the human checks/unchecks maps to a
// wildcard transition rule {from:"*", to:<state>} — so the selector is a
// human-readable view over the lower-level Transitions list, not a second
// storage model. The order here is the order the UI renders. It mirrors
// the default DefaultConfig().Notifications.Transitions destinations.
var NotifyTypes = []string{
	"idle",
	"awaiting_permission",
	"awaiting_input",
	"error",
	"exited",
}

// IsNotifyType reports whether to is one of the canonical NotifyTypes the
// per-type selector manages. Transitions to any other state (or with a
// non-wildcard From) are "advanced" rules the selector leaves untouched.
func IsNotifyType(to string) bool {
	return slices.Contains(NotifyTypes, to)
}

// NotifyTypeEnabled reports whether the friendly per-type checkbox for the
// destination state `to` is on — i.e. whether a wildcard rule {from:"*",
// to:to} is present in Transitions. A from-specific rule (e.g.
// {from:"working", to:"idle"}) is an advanced rule and does NOT light the
// checkbox; it is preserved untouched by SetNotifyType.
func (c *NotificationConfig) NotifyTypeEnabled(to string) bool {
	if c == nil {
		return false
	}
	for _, r := range c.Transitions {
		if r.From == "*" && r.To == to {
			return true
		}
	}
	return false
}

// SetNotifyType turns the friendly per-type notification on/off for the
// destination state `to` by adding or removing the single wildcard rule
// {from:"*", to:to}. Every other rule — from-specific rules and rules to
// non-canonical destinations — round-trips untouched, so the checklist and
// the raw "Advanced" transitions editor never clobber each other. on=true
// is idempotent (a duplicate wildcard rule is never added); on=false drops
// every wildcard rule for that destination.
func (c *NotificationConfig) SetNotifyType(to string, on bool) {
	if c == nil {
		return
	}
	// Rebuild without any wildcard rule for this destination; fresh
	// backing array (cap 0) so we never mutate a slice the caller may
	// still be aliasing.
	kept := make([]TransitionRule, 0, len(c.Transitions)+1)
	for _, r := range c.Transitions {
		if r.From == "*" && r.To == to {
			continue
		}
		kept = append(kept, r)
	}
	if on {
		kept = append(kept, TransitionRule{From: "*", To: to})
	}
	c.Transitions = kept
}

// MergeDefaultTypes additively ensures every currently-supported
// notification category (NotifyTypes) has its wildcard rule {from:"*",
// to:<type>} present, adding any that are missing and returning the
// destinations it added (in NotifyTypes order; nil if none). Existing
// rules — including from-specific "advanced" rules — and the
// cooldown/command/human-message settings are left untouched. This is how
// `tclaude setup` picks up categories introduced in a newer tclaude
// version without overwriting the user's other notification choices. A
// nil receiver returns nil.
func (c *NotificationConfig) MergeDefaultTypes() []string {
	if c == nil {
		return nil
	}
	var added []string
	for _, ty := range NotifyTypes {
		if !c.NotifyTypeEnabled(ty) {
			c.SetNotifyType(ty, true)
			added = append(added, ty)
		}
	}
	return added
}

// HumanMessagesIntent reports the human-messages preference independent of
// the master Enabled switch: it is the value the per-type "Sends me a
// message" checkbox should show. Unset (nil) defaults ON, matching
// NotifyHumanMessages's within-enabled default; only an explicit false is
// off. Distinct from NotifyHumanMessages, which additionally ANDs Enabled
// (the effective "should this banner fire" decision).
func (c *NotificationConfig) HumanMessagesIntent() bool {
	if c == nil {
		return true
	}
	return c.HumanMessages == nil || *c.HumanMessages
}
