package agentd

import (
	"strings"
	"testing"
)

func TestDashboardAssets_OpenCodeLegacyPricingCutoffWired(t *testing.T) {
	for _, needle := range []string{
		`id="cfg-opencode-legacy-long-context-pricing-cutoff"`,
		`placeholder="272000"`,
		"opencode.legacy_long_context_pricing_cutoff",
		"Explicit context tiers from OpenCode's provider catalog always take precedence",
		"cfgPositiveSafeInt(",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — OpenCode legacy pricing cutoff wiring broken", needle)
		}
	}
}
