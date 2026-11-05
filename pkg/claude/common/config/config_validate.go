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
func Validate(c *Config) []string {
	if c == nil {
		return []string{"config is nil"}
	}
	var errs []string
	if name, named := AskSandboxProfileName(c.AskSandboxSetting()); named {
		if _, err := sandboxpolicy.Normalize(sandboxpolicy.Profile{Name: name}); err != nil {
			errs = append(errs, "ask.sandbox: choose auto, global, off, or a valid sandbox profile name: "+err.Error())
		}
	}
	if c.StatusSnapshot != nil && (c.StatusSnapshot.FreshnessMS < 0 || c.StatusSnapshot.FreshnessMS > 60000) {
		errs = append(errs, "status_snapshot.freshness_ms must be 0 (default) or 1..60000")
	}
	errs = append(errs, c.Updates.Validate()...)
	if c.Dashboard != nil {
		if len(c.Dashboard.Keybindings) > maxKeybindings {
			errs = append(errs, fmt.Sprintf("dashboard.keybindings has more than %d entries", maxKeybindings))
		}
		for id, chord := range c.Dashboard.Keybindings {
			if !validKeybinding(id, chord) {
				errs = append(errs, fmt.Sprintf("dashboard.keybindings: %q → %q is not a shortcut id and a chord like Ctrl+Alt+KeyK", id, chord))
			}
		}
	}
	if f := c.Federation; f != nil {
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
	}
	errs = append(errs, validateCapacity(c.Capacity)...)
	errs = append(errs, validateGitProxy(c)...)
	if h := c.Host; h != nil {
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
	}

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
		if a.AWBProxy != nil && len(a.AWBProxy.ReadyPolling) > 0 {
			seen := make(map[string]string, len(a.AWBProxy.ReadyPolling))
			for key := range a.AWBProxy.ReadyPolling {
				normalized := strings.ToLower(strings.TrimSpace(key))
				if previous, ok := seen[normalized]; ok {
					errs = append(errs, fmt.Sprintf("agent.awb_proxy.ready_polling process names %q and %q normalize to the same process %q", previous, key, normalized))
					continue
				}
				seen[normalized] = key
			}
			processes := make(map[string][]AWBReadyPollingConfig)
			for _, polling := range a.AWBProxy.ReadyPolling {
				workspace := strings.ToLower(strings.TrimSpace(polling.Workspace))
				for _, other := range processes[workspace] {
					if len(polling.Labels) == 0 || len(other.Labels) == 0 || stringSlicesOverlapFold(polling.Labels, other.Labels) {
						errs = append(errs, fmt.Sprintf("agent.awb_proxy.ready_polling processes for workspace %q must have disjoint non-empty label filters", workspace))
						break
					}
				}
				processes[workspace] = append(processes[workspace], polling)
			}
		}
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
		if f := *cc.EstimateFactor; f <= 0 || f > maxCostEstimateFactor || math.IsNaN(f) || math.IsInf(f, 0) {
			errs = append(errs, fmt.Sprintf("cost.estimate_factor %g is out of range (>0 and ≤%g) — it is a display multiplier, e.g. 1.1 for +10%%", f, maxCostEstimateFactor))
		}
	}

	if cc := c.Cost; cc != nil {
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
	if prompt := c.StewardStartupPrompt(); len(prompt) > MaxStewardStartupPromptLen {
		errs = append(errs, fmt.Sprintf("steward.startup_prompt: must be at most %d bytes", MaxStewardStartupPromptLen))
	} else if strings.ContainsFunc(prompt, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		errs = append(errs, "steward.startup_prompt: must not contain control characters")
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
