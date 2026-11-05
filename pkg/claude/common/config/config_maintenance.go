package config

import (
	"regexp"
	"strings"

	"github.com/tofutools/tclaude/pkg/common"
)

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
