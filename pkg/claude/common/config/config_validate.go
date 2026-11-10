package config

import (
	"fmt"
	"math"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Validate checks a Config for problems that would make it unsafe or
// nonsensical to persist, returning a list of human-readable error
// strings (empty when the config is acceptable). It is the gatekeeper
// for the dashboard's visual config editor: every problem is reported
// at once so the human fixes them in a single pass instead of one
// failed save at a time. Load() is deliberately more lenient — it
// degrades a bad value to a default and carries on — but a human
// editing config through the dashboard wants to be told.
//
// Each section has its own validator; they run in a fixed order, which is
// the order the editor lists the problems in.
func Validate(c *Config) []string {
	if c == nil {
		return []string{"config is nil"}
	}
	var errs []string
	errs = append(errs, validateAsk(c)...)
	errs = append(errs, validateStatusSnapshot(c.StatusSnapshot)...)
	errs = append(errs, c.Updates.Validate()...)
	errs = append(errs, validateDashboardKeybindings(c.Dashboard)...)
	errs = append(errs, validateFederation(c.Federation)...)
	errs = append(errs, validateCapacity(c.Capacity)...)
	errs = append(errs, validateGitProxy(c)...)
	errs = append(errs, validateHost(c.Host)...)
	errs = append(errs, validateLogLevel(c.LogLevel)...)
	errs = append(errs, validatePreCompactGuard(c.PreCompactGuard)...)
	errs = append(errs, validateRateLimit(c.RateLimit)...)
	errs = append(errs, validateOpenCode(c.OpenCode)...)
	errs = append(errs, validateClaudeCleanupPeriodDays(c.ClaudeCleanupPeriodDays)...)
	errs = append(errs, validateRemoteAccess(c.RemoteAccess)...)
	errs = append(errs, validateNotifications(c.Notifications)...)
	errs = append(errs, validateAgent(c.Agent)...)
	errs = append(errs, validateCost(c.Cost)...)
	errs = append(errs, validateUsage(c.Usage)...)
	errs = append(errs, validateClaudeResume(c.ClaudeResume)...)
	errs = append(errs, validateSlop(c.Slop)...)
	errs = append(errs, validateLogRotation(c.LogRotation)...)
	errs = append(errs, validateFocusTile(c.Focus)...)
	errs = append(errs, validateTerminalAttach(c.Dashboard)...)
	errs = append(errs, validateSteward(c)...)
	errs = append(errs, validateTUI(c.TUI)...)
	errs = append(errs, validateFeatures(c.Features)...)
	return errs
}

func validateAsk(c *Config) []string {
	if name, named := AskSandboxProfileName(c.AskSandboxSetting()); named {
		if _, err := sandboxpolicy.Normalize(sandboxpolicy.Profile{Name: name}); err != nil {
			return []string{"ask.sandbox: choose auto, global, off, or a valid sandbox profile name: " + err.Error()}
		}
	}
	return nil
}

func validateStatusSnapshot(s *StatusSnapshotConfig) []string {
	if s != nil && (s.FreshnessMS < 0 || s.FreshnessMS > 60000) {
		return []string{"status_snapshot.freshness_ms must be 0 (default) or 1..60000"}
	}
	return nil
}

func validateDashboardKeybindings(d *DashboardConfig) []string {
	if d == nil {
		return nil
	}
	var errs []string
	if len(d.Keybindings) > maxKeybindings {
		errs = append(errs, fmt.Sprintf("dashboard.keybindings has more than %d entries", maxKeybindings))
	}
	for id, chord := range d.Keybindings {
		if !validKeybinding(id, chord) {
			errs = append(errs, fmt.Sprintf("dashboard.keybindings: %q → %q is not a shortcut id and a chord like Ctrl+Alt+KeyK", id, chord))
		}
	}
	return errs
}

func validateFederation(f *FederationConfig) []string {
	if f == nil {
		return nil
	}
	var errs []string
	if f.Teleport != nil {
		if err := f.Teleport.Backup.Validate(); err != nil {
			errs = append(errs, err.Error())
		}
		l := f.Teleport.Limits.Effective()
		if l.Hour < 1 || l.Day < 1 || l.Chain < 1 || l.Chain > 128 || l.RevisitMinutes < 0 || l.RevisitMinutes > 525600 {
			errs = append(errs, "federation.teleport.limits must have positive rates, per_chain 1..128, and revisit_minutes 0..525600")
		}
	}
	if f.Health != nil {
		if e := f.Health.Defaults.Validate(); e != nil {
			errs = append(errs, "federation.health.defaults: "+e.Error())
		}
		for id, p := range f.Health.Peers {
			if !proto.ValidInstanceID(id) {
				errs = append(errs, "federation.health.peers requires immutable instance IDs")
			}
			if e := p.Validate(); e != nil {
				errs = append(errs, "federation.health.peers: "+e.Error())
			}
		}
	}
	if f.MaxLiveAgents < 0 {
		errs = append(errs, "federation.max_live_agents must be nonnegative (zero is unlimited)")
	}
	if len(f.NodeLabels) > 64 {
		errs = append(errs, "federation.node_labels allows at most 64 labels")
	}
	for _, label := range f.NodeLabels {
		valid := len(label) > 0 && len(label) <= 64
		for _, r := range label {
			allowed := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
			if !allowed {
				valid = false
			}
		}
		if !valid {
			errs = append(errs, "federation.node_labels must use 1..64 letters, digits, dot, dash or underscore")
			break
		}
	}
	return errs
}

func validateHost(h *HostConfig) []string {
	if h == nil {
		return nil
	}
	var errs []string
	for _, entry := range []struct {
		name  string
		value *float64
		max   float64
	}{
		{"warn_load_per_core", h.WarnLoadPerCore, 0},
		{"warn_ram_available_percent", h.WarnRAMAvailablePercent, 100},
		{"warn_disk_available_percent", h.WarnDiskAvailablePercent, 100},
	} {
		if entry.value != nil && (math.IsNaN(*entry.value) || math.IsInf(*entry.value, 0) || *entry.value < 0 || (entry.max > 0 && *entry.value > entry.max)) {
			errs = append(errs, "host."+entry.name+" must be finite and nonnegative (percent thresholds must be at most 100)")
		}
	}
	for _, dir := range h.WorkDirs {
		if !filepath.IsAbs(dir) {
			errs = append(errs, "host.work_dirs must contain absolute paths")
		}
	}
	return errs
}

func validateLogLevel(level string) []string {
	switch level {
	case "", "debug", "info", "warn", "error":
		return nil
	default:
		return []string{fmt.Sprintf("log_level %q is not one of debug, info, warn, error", level)}
	}
}

func validatePreCompactGuard(g *PreCompactGuardConfig) []string {
	if g == nil {
		return nil
	}
	var errs []string
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
	return errs
}

func validateRateLimit(r *RateLimitConfig) []string {
	if r == nil {
		return nil
	}
	var errs []string
	if v := r.FiveHourPercentMaxUsed; v <= 0 || v > 100 {
		errs = append(errs, fmt.Sprintf("ratelimit.five_hour_percent_max_used %g is out of range (>0 and ≤100)", v))
	}
	if v := r.SevenDayPercentMaxUsed; v <= 0 || v > 100 {
		errs = append(errs, fmt.Sprintf("ratelimit.seven_day_percent_max_used %g is out of range (>0 and ≤100)", v))
	}
	return errs
}

func validateOpenCode(oc *OpenCodeConfig) []string {
	if oc != nil && oc.LegacyLongContextPricingCutoff != nil &&
		*oc.LegacyLongContextPricingCutoff <= 0 {
		return []string{fmt.Sprintf(
			"opencode.legacy_long_context_pricing_cutoff %d must be positive (default %d)",
			*oc.LegacyLongContextPricingCutoff, DefaultOpenCodeLegacyLongContextPricingCutoff)}
	}
	return nil
}

// validateClaudeCleanupPeriodDays: claude_cleanup_period_days maps to Claude
// Code's cleanupPeriodDays, which Claude Code requires to be ≥ 1. 0 / absent
// is our "leave the key alone" sentinel; a negative value is a typo we reject
// so it never silently becomes a no-op or reaches settings.json.
func validateClaudeCleanupPeriodDays(days int) []string {
	if days < 0 {
		return []string{fmt.Sprintf("claude_cleanup_period_days %d must not be negative (0 = leave Claude Code's default; a positive day count overrides it)", days)}
	}
	return nil
}

// validateRemoteAccess checks the bind only when the listener is actually
// enabled: a half-typed bind left behind while disabled is harmless (nothing
// starts), so it must not block an unrelated save.
func validateRemoteAccess(r *RemoteAccessConfig) []string {
	if r == nil || !r.Enabled {
		return nil
	}
	if r.Bind == "" {
		return []string{"remote_access.enabled is set but remote_access.bind is empty; set a bind address (e.g. 0.0.0.0:8443)"}
	} else if _, port, err := net.SplitHostPort(r.Bind); err != nil {
		// A non-empty bind must be a host:port the listener can actually
		// bind to — net.Listen("tcp", …) needs the port. Catch a missing
		// port here (as a clean save-time error) rather than letting
		// startRemoteServer fail at boot and only log it.
		return []string{fmt.Sprintf("remote_access.bind %q is not a valid host:port (e.g. 0.0.0.0:8443): %v", r.Bind, err)}
	} else if n, perr := strconv.Atoi(port); perr != nil || n < 1 || n > 65535 {
		// Require an explicit numeric port the operator can dial. A named
		// service ("https") or 0 would technically listen — but 0 binds a
		// random OS-assigned port nobody can reach by URL, and a name is a
		// surprise; reject both so the configured port is the one served.
		return []string{fmt.Sprintf("remote_access.bind %q needs a numeric port 1–65535 (got %q)", r.Bind, port)}
	}
	return nil
}

func validateNotifications(n *NotificationConfig) []string {
	if n == nil {
		return nil
	}
	var errs []string
	if n.CooldownSeconds < 0 {
		errs = append(errs, "notifications.cooldown_seconds must not be negative")
	}
	if !IsNotifyDelivery(n.Delivery) {
		errs = append(errs, fmt.Sprintf("notifications.delivery %q must be one of %q, %q, %q (or absent)",
			n.Delivery, NotifyDeliveryOS, NotifyDeliveryBrowser, NotifyDeliveryBoth))
	}
	for i, tr := range n.Transitions {
		if tr.From == "" || tr.To == "" {
			errs = append(errs, fmt.Sprintf("notifications.transitions[%d] needs both from and to (use \"*\" for any state)", i))
		}
	}
	return errs
}

func validateAgent(a *AgentConfig) []string {
	if a == nil {
		return nil
	}
	var errs []string
	errs = append(errs, validateAWBProxy(a.AWBProxy)...)
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
	errs = append(errs, validateAgentDashboardListener(a)...)
	errs = append(errs, validateContextNudge(a.ContextNudge)...)
	errs = append(errs, validateRetiredCleanup(a.RetiredCleanup)...)
	errs = append(errs, validateSudo(a.Sudo)...)
	return errs
}

func validateAWBProxy(p *AWBProxyConfig) []string {
	if p == nil || len(p.ReadyPolling) == 0 {
		return nil
	}
	var errs []string
	seen := make(map[string]string, len(p.ReadyPolling))
	for key := range p.ReadyPolling {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if previous, ok := seen[normalized]; ok {
			errs = append(errs, fmt.Sprintf("agent.awb_proxy.ready_polling process names %q and %q normalize to the same process %q", previous, key, normalized))
			continue
		}
		seen[normalized] = key
	}
	processes := make(map[string][]AWBReadyPollingConfig)
	for _, polling := range p.ReadyPolling {
		workspace := strings.ToLower(strings.TrimSpace(polling.Workspace))
		for _, other := range processes[workspace] {
			if len(polling.Labels) == 0 || len(other.Labels) == 0 || stringSlicesOverlapFold(polling.Labels, other.Labels) {
				errs = append(errs, fmt.Sprintf("agent.awb_proxy.ready_polling processes for workspace %q must have disjoint non-empty label filters", workspace))
				break
			}
		}
		processes[workspace] = append(processes[workspace], polling)
	}
	return errs
}

func validateAgentDashboardListener(a *AgentConfig) []string {
	var errs []string
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
	return errs
}

func validateContextNudge(cn *ContextNudgeConfig) []string {
	if cn == nil {
		return nil
	}
	var errs []string
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
	return errs
}

func validateRetiredCleanup(rc *RetiredCleanupConfig) []string {
	if rc == nil {
		return nil
	}
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
		return []string{fmt.Sprintf("agent.retired_cleanup.after_days %d is out of range (must be %d–%d — it is the number of days an agent stays retired before it is permanently deleted)", rc.AfterDays, lo, MaxRetiredCleanupAfterDays)}
	}
	return nil
}

func validateCost(cc *CostConfig) []string {
	if cc == nil {
		return nil
	}
	var errs []string
	if cc.EstimateFactor != nil {
		if f := *cc.EstimateFactor; f <= 0 || f > maxCostEstimateFactor || math.IsNaN(f) || math.IsInf(f, 0) {
			errs = append(errs, fmt.Sprintf("cost.estimate_factor %g is out of range (>0 and ≤%g) — it is a display multiplier, e.g. 1.1 for +10%%", f, maxCostEstimateFactor))
		}
	}
	for name, value := range map[string]float64{"daily_limit_usd": cc.DailyLimitUSD, "monthly_limit_usd": cc.MonthlyLimitUSD} {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			errs = append(errs, "cost."+name+" must be a finite non-negative USD amount (0 disables)")
		}
	}
	switch strings.TrimSpace(cc.GeminiBilling) {
	case "", GeminiBillingAuto, GeminiBillingAPI, GeminiBillingSubscription:
	default:
		errs = append(errs, fmt.Sprintf("cost.gemini_billing %q is not one of %q, %q or %q",
			cc.GeminiBilling, GeminiBillingAuto, GeminiBillingAPI, GeminiBillingSubscription))
	}
	for name, f := range cc.HarnessFactors {
		if !ValidCostFactorHarness(name) {
			errs = append(errs, fmt.Sprintf("cost.harness_factors.%s: unknown harness", name))
		}
		if f <= 0 || f > maxCostEstimateFactor || math.IsNaN(f) || math.IsInf(f, 0) {
			errs = append(errs, fmt.Sprintf("cost.harness_factors.%s %g is out of range (>0 and ≤%g)", name, f, maxCostEstimateFactor))
		}
	}
	return errs
}

// validateUsage: usage.idle_timeout is a Go duration string; reject anything
// that doesn't parse or isn't positive so the human is told, rather than
// letting ResolvedUsageIdleTimeout silently fall back to the default.
func validateUsage(uc *UsageConfig) []string {
	if uc == nil || uc.IdleTimeout == "" {
		return nil
	}
	if d, err := time.ParseDuration(uc.IdleTimeout); err != nil {
		return []string{fmt.Sprintf("usage.idle_timeout %q is not a valid duration (e.g. %q for 3 days, or %q) — it is how long the last-known usage reading stays on the dashboard after its source goes idle", uc.IdleTimeout, "72h", "30m")}
	} else if d <= 0 {
		return []string{fmt.Sprintf("usage.idle_timeout %q must be positive — it is how long the last-known usage reading stays on the dashboard after its source goes idle", uc.IdleTimeout)}
	}
	return nil
}

// validateClaudeResume: the resume thresholds are minute / token counts
// handed verbatim to Claude Code, which parses them as non-negative integers;
// a negative value is meaningless (and CC would reject it), so flag it rather
// than inject a var CC ignores. 0 is allowed — it FORCES the prompt for every
// resume, the deliberate inverse of the suppress sentinel.
func validateClaudeResume(cr *ClaudeResumeConfig) []string {
	if cr == nil {
		return nil
	}
	var errs []string
	if cr.ThresholdMinutes != nil && *cr.ThresholdMinutes < 0 {
		errs = append(errs, fmt.Sprintf("claude_resume.threshold_minutes %d must not be negative (use a large value to suppress the prompt, 0 to always show it)", *cr.ThresholdMinutes))
	}
	if cr.TokenThreshold != nil && *cr.TokenThreshold < 0 {
		errs = append(errs, fmt.Sprintf("claude_resume.token_threshold %d must not be negative (use a large value to suppress the prompt, 0 to always show it)", *cr.TokenThreshold))
	}
	return errs
}

func validateSlop(s *SlopConfig) []string {
	if s == nil {
		return nil
	}
	var errs []string
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
	return errs
}

func validateLogRotation(lr *LogRotationConfig) []string {
	if lr == nil {
		return nil
	}
	var errs []string
	if lr.MaxSize != "" {
		if _, err := common.ParseSize(lr.MaxSize); err != nil {
			errs = append(errs, fmt.Sprintf("log_rotation.max_size %q is not a valid size (e.g. \"10MiB\", \"50m\", or \"0\" to disable)", lr.MaxSize))
		}
	}
	if lr.Keep < 0 {
		errs = append(errs, fmt.Sprintf("log_rotation.keep %d must not be negative (0 = built-in default)", lr.Keep))
	}
	return errs
}

func validateFocusTile(f *FocusConfig) []string {
	if f == nil || f.Tile == nil {
		return nil
	}
	var errs []string
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
	return errs
}

func validateTerminalAttach(d *DashboardConfig) []string {
	if d == nil || d.TerminalAttach == nil {
		return nil
	}
	var errs []string
	t := d.TerminalAttach
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
	return errs
}

func validateSteward(c *Config) []string {
	var errs []string
	switch c.StewardReads() {
	case StewardReadsOperator, StewardReadsFleet:
	default:
		errs = append(errs, fmt.Sprintf("steward.reads %q is not one of %s, %s",
			c.StewardReads(), StewardReadsOperator, StewardReadsFleet))
	}
	switch c.StewardWrites() {
	case StewardWritesNone, StewardWritesMessaging, StewardWritesWorkers:
	default:
		errs = append(errs, fmt.Sprintf("steward.writes %q is not one of %s, %s, %s",
			c.StewardWrites(), StewardWritesNone, StewardWritesMessaging, StewardWritesWorkers))
	}
	switch c.StewardSourceDownload() {
	case StewardSourceAsk, StewardSourceEachVersion, StewardSourceNever:
	default:
		errs = append(errs, fmt.Sprintf("steward.source_download %q is not one of %s, %s, %s",
			c.StewardSourceDownload(), StewardSourceAsk, StewardSourceEachVersion, StewardSourceNever))
	}
	if name := c.stewardNameSetting(); name != "" && !validStewardName(name) {
		errs = append(errs, fmt.Sprintf("steward.name: must be at most %d characters: letters and digits, with single spaces, - or _ between them", MaxStewardNameLen))
	}
	if c.Steward != nil {
		if _, ok := expandStewardDir(c.Steward.Dir); !ok {
			errs = append(errs, fmt.Sprintf("steward.dir %q: must be an absolute path or start with ~/", c.Steward.Dir))
		} else if strings.ContainsFunc(c.Steward.Dir, unicode.IsControl) {
			errs = append(errs, "steward.dir: must not contain control characters")
		}
	}
	if prompt := c.StewardStartupPrompt(); len(prompt) > MaxStewardStartupPromptLen {
		errs = append(errs, fmt.Sprintf("steward.startup_prompt: must be at most %d bytes", MaxStewardStartupPromptLen))
	} else if strings.ContainsFunc(prompt, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		errs = append(errs, "steward.startup_prompt: must not contain control characters")
	}
	return errs
}

// validateTUI: an empty/absent scheme resolves to the default; only a
// non-empty value outside the known set is worth flagging (mirrors
// focus.tile.layout).
func validateTUI(t *TUIConfig) []string {
	if t != nil && t.ColorScheme != "" && normalizeTUIColorScheme(t.ColorScheme) == "" {
		return []string{fmt.Sprintf("tui.color_scheme %q is not one of %s, %s",
			t.ColorScheme, TUIColorSchemeDefault, TUIColorSchemeHighContrast)}
	}
	return nil
}

func validateFeatures(f *FeaturesConfig) []string {
	if f == nil {
		return nil
	}
	switch f.GroupAttachments {
	case "", GroupAttachmentsOff, GroupAttachmentsFloat, GroupAttachmentsFixed:
		return nil
	default:
		return []string{fmt.Sprintf(
			"features.group_attachments %q is not one of %s, %s, %s",
			f.GroupAttachments, GroupAttachmentsOff, GroupAttachmentsFloat, GroupAttachmentsFixed)}
	}
}

func stringSlicesOverlapFold(a, b []string) bool {
	seen := make(map[string]struct{}, len(a))
	for _, value := range a {
		seen[value] = struct{}{}
	}
	for _, value := range b {
		if _, ok := seen[value]; ok {
			return true
		}
	}
	return false
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
