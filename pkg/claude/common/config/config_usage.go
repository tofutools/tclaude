package config

import (
	"math"
	"strings"
	"time"
)

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
