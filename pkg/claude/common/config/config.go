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

	// Cost holds display-only cost adjustments — see CostConfig. Absent →
	// no adjustment (the recorded figures are shown verbatim).
	Cost *CostConfig `json:"cost,omitempty"`

	// Ask holds the default model/effort profile for `tclaude ask` — see
	// AskConfig. Absent / blank fields fall back to the built-in default
	// constants (DefaultAskModel + DefaultAskEffort); see
	// ResolvedAskProfile.
	Ask *AskConfig `json:"ask,omitempty"`

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

	// Dashboard holds display toggles for the agentd web dashboard that
	// don't belong to the slop / cost / notification blocks — see
	// DashboardConfig. Absent → all defaults.
	Dashboard *DashboardConfig `json:"dashboard,omitempty"`

	// Session holds session-launch tuning — currently the tmux-session
	// naming style. Absent → all defaults (short id-prefix tmux names).
	Session *SessionConfig `json:"session,omitempty"`
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
//	           can be opened. This is the default.
//	"web"    — open the console as an in-browser terminal pane in the
//	           dashboard's own Terminals tab, without touching the OS windowing
//	           system — the same surface the dedicated "web term" / "web window"
//	           buttons already always use.
//
// An empty / unknown value falls back to the default (see DefaultTerminal).
const (
	DefaultTerminalNative = "native"
	DefaultTerminalWeb    = "web"
)

// DashboardConfig holds display toggles for the agentd web dashboard.
type DashboardConfig struct {
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
	// DefaultTerminal selects how the dashboard's per-agent focus / open-window
	// / open-terminal actions open a console — one of DefaultTerminal{Native,
	// Web}. "native" (the default) pops a native OS window (falling back to an
	// in-browser PTY only when it can't); "web" opens an in-browser terminal
	// pane in the dashboard's Terminals tab instead, the same surface the
	// dedicated "web term" / "web window" buttons use. Empty / unknown →
	// default (native). The dashboard reads the resolved value off the snapshot
	// each poll and routes its focus/open actions accordingly. The dedicated
	// web buttons and the native-window bulk "windows…" modal are unaffected.
	// See (*Config).DefaultTerminal.
	DefaultTerminal string `json:"default_terminal,omitempty"`
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
	// resolved live at ask time (db.GetSpawnProfile); a deleted/renamed
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
// this true to reveal it in WHAT-IF mode — the estimated pay-per-token-
// equivalent cost (Claude Code's client-side total_cost_usd, captured into
// virtual_cost_usd), clearly flagged as hypothetical. Default false = hide on
// subscription. Editable from the dashboard's Config tab.
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

// DefaultTerminal reports how the dashboard's per-agent focus / open-window /
// open-terminal actions open a console — config dashboard.default_terminal.
// Default "native" (absent block / key or an unknown value): pop a native OS
// window (with the usual in-browser fallback when none can be opened). "web"
// routes those actions to an in-browser terminal pane in the dashboard's
// Terminals tab instead. Nil-safe on the receiver so callers need no guard.
func (c *Config) DefaultTerminal() string {
	if c != nil && c.Dashboard != nil {
		if s := normalizeDefaultTerminal(c.Dashboard.DefaultTerminal); s != "" {
			return s
		}
	}
	return DefaultTerminalNative
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
// "member.redesignate", "agent.spawn". Unknown slugs are ignored
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
// for the case where the human grants an AGENT the `groups.spawn`
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
	DefaultPermissions        []string            `json:"default_permissions,omitempty"`
	Sudo                      *SudoConfig         `json:"sudo,omitempty"`
	ContextNudge              *ContextNudgeConfig `json:"context_nudge,omitempty"`
	AutoLaunchDashboard       bool                `json:"auto_launch_dashboard,omitempty"`
	DisableTray               bool                `json:"disable_tray,omitempty"` // suppress the agentd tray icon; --no-tray ORs with it
	BranchHistoryPREnrichment bool                `json:"branch_history_pr_enrichment,omitempty"`
	CloneCooldown             string              `json:"clone_cooldown,omitempty"`
	SpawnGroupRestriction     *bool               `json:"spawn_group_restriction,omitempty"`
	SpawnAllowedGroups        []string            `json:"spawn_allowed_groups,omitempty"`
	SpawnMaxPerHour           *int                `json:"spawn_max_per_hour,omitempty"`

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
	// The persisted secret is stored by agentd (see
	// agentd.loadOrCreateOperatorToken): the OS keychain when one is
	// reachable (macOS Keychain / Linux Secret Service / Windows
	// Credential Manager), else a 0600 ~/.tclaude/operator_token file.
	// The secret is deliberately NOT held in this config file — config.json
	// is plaintext and shows up in the Config-tab diff / backups, and the
	// agent sandbox already denies reads to ~/.tclaude (so the file
	// fallback keeps the same threat model as the in-memory token).
	PersistOperatorToken bool `json:"persist_operator_token,omitempty"`
}

// DefaultSpawnInlineMaxChars is the fallback briefing-inline threshold (runes)
// used when AgentConfig.SpawnInlineMaxChars is unset. Roughly a few paragraphs
// — long enough to inline a typical short task brief on the first turn, short
// enough that a genuinely large brief still routes to the inbox rather than
// ballooning the launch command. See agentd.spawnInlineMaxChars.
const DefaultSpawnInlineMaxChars = 2000

// ContextNudgeConfig controls the opt-in "consider reincarnating"
// nudge that fires as a long-running agent's context fills. Off by
// default — a fresh daemon shouldn't start typing into the agent's
// pane until the human signs up for it.
//
// Threshold ladder: starting at MinPct, every IntervalPct, capped at
// 90. So MinPct=30 + IntervalPct=10 → fires at 30, 40, 50, 60, 70,
// 80, 90. MinPct=50 + IntervalPct=20 → 50, 70, 90.
//
// The daemon tracks per-session "highest threshold already fired"
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

// ConfigDir returns the tclaude config directory (~/.tclaude).
func ConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tclaude")
}

// ConfigPath returns the path to the config file (~/.tclaude/config.json).
func ConfigPath() string {
	return filepath.Join(ConfigDir(), "config.json")
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
	dir := ConfigDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
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
	return os.Rename(tmpName, ConfigPath())
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
		for i, tr := range c.Notifications.Transitions {
			if tr.From == "" || tr.To == "" {
				errs = append(errs, fmt.Sprintf("notifications.transitions[%d] needs both from and to (use \"*\" for any state)", i))
			}
		}
	}

	if c.Agent != nil {
		a := c.Agent
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
