package config

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/common"
)

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
	if c == nil {
		return false
	}
	changed := false
	if c.Steward != nil {
		changed = renamePermissionList(&c.Steward.DefaultPermissions, semanticProxyPermissionRenames, groupPermissionRenames)
	}
	if c.Agent == nil {
		return changed
	}
	if renamePermissionList(&c.Agent.DefaultPermissions, semanticProxyPermissionRenames, groupPermissionRenames) {
		changed = true
	}
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
	// Keep the keys this version does not model (see preserve.go): the
	// typed decode that produced config never saw them.
	if old, err := os.ReadFile(target); err == nil {
		if data, err = mergeUnknownKeys(data, old); err != nil {
			return err
		}
	}

	return writeConfigLocked(target, data)
}

// writeConfigLocked writes data over the config file at target atomically.
// The caller holds saveMu.
func writeConfigLocked(target string, data []byte) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
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
	if err := os.Rename(tmpName, target); err != nil {
		return err
	}
	invalidateShared()
	return nil
}
