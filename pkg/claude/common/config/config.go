// Package config provides configuration loading for tclaude.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/common"
)

// Config represents the tclaude configuration file structure.
type Config struct {
	Notifications      *NotificationConfig `json:"notifications,omitempty"`
	AutoCompactPercent *int                `json:"auto_compact_percent,omitempty"`
	LogLevel           string              `json:"log_level,omitempty"`
	RecordHooks        bool                `json:"record_hooks,omitempty"`
	RateLimit          *RateLimitConfig    `json:"ratelimit,omitempty"`
	Agent              *AgentConfig        `json:"agent,omitempty"`

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
	// volumes. Absent block / absent keys mean full volume — see
	// ResolvedSlopVolumes.
	Slop *SlopConfig `json:"slop,omitempty"`
}

// SlopConfig holds the slop-mode audio knobs. Both volumes are percent
// (0–100) of the mode's built-in full level: MusicVolume scales the
// Vegas lounge radio, EffectsVolume scales the synthesized casino FX.
// Pointers so "absent" (default 100) is distinguishable from an
// explicit 0 (silent but not muted — the master 🔇/🔊 switch is a
// separate localStorage-persisted preference in the browser).
//
// Written by the dashboard's volume sliders via POST /api/slop/volumes;
// also round-trips through the Config tab like any other field.
type SlopConfig struct {
	MusicVolume   *int `json:"music_volume,omitempty"`
	EffectsVolume *int `json:"effects_volume,omitempty"`
}

// defaultSlopVolume is the effective volume for an absent slop volume
// key: 100% — entering slop mode is opting in to the full casino.
const defaultSlopVolume = 100

// ResolvedSlopVolumes returns the effective (music, effects) volumes in
// percent, defaulting each absent value to 100. A hand-edited
// out-of-range value is clamped to 0–100 — Validate reports it to the
// Config tab, but readers must still get a usable volume rather than
// handing 500% to the browser. Nil-safe on the receiver so callers
// need no guard.
func (c *Config) ResolvedSlopVolumes() (music, effects int) {
	music, effects = defaultSlopVolume, defaultSlopVolume
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
type FocusConfig struct {
	RaiseOnly bool `json:"raise_only,omitempty"`
}

// RaiseOnlyFocus reports whether window focus should be raise-only (raise
// an existing window but never open a fresh one). Nil-safe so callers
// need no guard; the absent default is false (open-on-focus).
func (c *Config) RaiseOnlyFocus() bool {
	if c == nil || c.Focus == nil {
		return false
	}
	return c.Focus.RaiseOnly
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
// hardcoded defaults. Per-conv overrides via Sudo.Overrides[] use
// selector-shaped keys (conv-id / title, with prefix match against
// title and conv-id) the historical permission_overrides block did.
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
	BranchHistoryPREnrichment bool                `json:"branch_history_pr_enrichment,omitempty"`
	CloneCooldown             string              `json:"clone_cooldown,omitempty"`
	SpawnGroupRestriction     *bool               `json:"spawn_group_restriction,omitempty"`
	SpawnAllowedGroups        []string            `json:"spawn_allowed_groups,omitempty"`
	SpawnMaxPerHour           *int                `json:"spawn_max_per_hour,omitempty"`
}

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

// MatchSudoOverride picks the SudoConfigOverride that applies to the
// caller (convID / title). Match shape mirrors the historical
// permission_overrides[<conv-id|prefix|title>] pattern: a key matches
// if it equals one of the two identifiers OR is a prefix of conv-id
// (≥8 chars) or title. The longest matching key wins so a more
// specific override beats a generic prefix. Returns nil when no key
// matches.
func (c *Config) MatchSudoOverride(convID, title string) *SudoConfigOverride {
	if c == nil || c.Agent == nil || c.Agent.Sudo == nil {
		return nil
	}
	var (
		bestKey string
		best    *SudoConfigOverride
	)
	for k, v := range c.Agent.Sudo.Overrides {
		if !sudoOverrideKeyMatches(k, convID, title) {
			continue
		}
		if len(k) > len(bestKey) {
			bestKey = k
			best = v
		}
	}
	return best
}

func sudoOverrideKeyMatches(key, convID, title string) bool {
	if key == "" {
		return false
	}
	if key == convID || key == title {
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
		c.Notifications = DefaultConfig().Notifications
	} else {
		if c.Notifications.CooldownSeconds == 0 {
			c.Notifications.CooldownSeconds = 5
		}
		if len(c.Notifications.Transitions) == 0 {
			c.Notifications.Transitions = DefaultConfig().Notifications.Transitions
		}
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

	if c.AutoCompactPercent != nil {
		if p := *c.AutoCompactPercent; p < 1 || p > 100 {
			errs = append(errs, fmt.Sprintf("auto_compact_percent %d is out of range (1–100)", p))
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
		errs = append(errs, validateSudo(a.Sudo)...)
	}

	if s := c.Slop; s != nil {
		if s.MusicVolume != nil && (*s.MusicVolume < 0 || *s.MusicVolume > 100) {
			errs = append(errs, fmt.Sprintf("slop.music_volume %d is out of range (0–100)", *s.MusicVolume))
		}
		if s.EffectsVolume != nil && (*s.EffectsVolume < 0 || *s.EffectsVolume > 100) {
			errs = append(errs, fmt.Sprintf("slop.effects_volume %d is out of range (0–100)", *s.EffectsVolume))
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
