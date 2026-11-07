package config

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// SnapshotsConfig configures the local agent snapshot library.
type SnapshotsConfig struct {
	// DefaultRetention is how long a snapshot is kept when created without
	// an explicit expiry, as a duration such as "720h" or "30d". Empty
	// keeps snapshots until removed.
	DefaultRetention string `json:"default_retention,omitempty"`
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
	// Dir is the directory a new Steward starts in: an absolute path, or one
	// under ~/. Empty means the shared scribe workdir, ~/.tclaude/scribe.
	Dir string `json:"dir,omitempty"`
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
	// StewardReadsFleet reads the fleet, the drawing boards and the dashboard view.
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

// StewardDir returns the directory a new Steward starts in, with a leading ~/
// expanded and the path cleaned, or "" for the default scribe workdir (also
// when the setting is not a usable path; validation reports that). Nil-safe.
func (c *Config) StewardDir() string {
	if c == nil || c.Steward == nil {
		return ""
	}
	dir, ok := expandStewardDir(c.Steward.Dir)
	if !ok {
		return ""
	}
	return dir
}

// expandStewardDir resolves a steward.dir value: "" stays "", "~" and "~/..."
// expand to the home directory, and anything else must be absolute.
func expandStewardDir(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", false
		}
		raw = filepath.Join(home, strings.TrimPrefix(raw, "~"))
	}
	if !filepath.IsAbs(raw) {
		return "", false
	}
	return filepath.Clean(raw), true
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
