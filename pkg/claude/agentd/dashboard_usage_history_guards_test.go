package agentd

import (
	"strings"
	"testing"
)

// The layout assertions in TestDashboardUsageHistoryPreactBoundary are only
// worth anything if they fail when the layout regresses. Earlier versions of
// these helpers passed vacuously on most regressions — reading only the first
// matching CSS rule (blind to the later override that actually wins in this
// flat stylesheet), matching a bare "auto" anywhere in a declaration block, and
// comparing string offsets without checking the markers existed. These cover
// the cases that were silently green.
func TestUsageLayoutGuardsFailOnRegressions(t *testing.T) {
	t.Run("css rules", func(t *testing.T) {
		css := ".usage-history-note {\n  margin: 18px 0 0; overflow-x: auto;\n}\n" +
			"@media (max-width: 600px) {\n  .usage-history-note { max-width: 900px; }\n}\n"
		rules := usageCSSRules(t, css, ".usage-history-note")
		if len(rules) != 2 {
			t.Fatalf("want the base rule and the @media override, got %d: %q", len(rules), rules)
		}
		if !strings.Contains(rules[1], "max-width") {
			t.Error("a later @media override must be visible, since it is the rule that wins")
		}
		if usageMarginIsAuto(rules[0]) {
			t.Error("overflow-x: auto must not be read as an auto margin")
		}
	})

	t.Run("auto margin detection", func(t *testing.T) {
		for _, tc := range []struct {
			rule string
			want bool
		}{
			{" margin: 0 auto 14px; ", true},
			{" margin-inline: auto; ", true},
			{" margin: 18px 0 0; ", false},
			{" overflow-x: auto; height: auto; ", false},
			{" margin: 8px 0 0; padding: 0 10px; ", false},
		} {
			if got := usageMarginIsAuto(tc.rule); got != tc.want {
				t.Errorf("usageMarginIsAuto(%q) = %v, want %v", tc.rule, got, tc.want)
			}
		}
	})

	t.Run("note placement", func(t *testing.T) {
		for _, tc := range []struct {
			island string
			want   string
		}{
			{"usage-series-grid ... usage-history-note", usageNoteBelowGrid},
			{"usage-history-note ... usage-series-grid", usageNoteAboveGrid},
			// Each of these passed as "below grid" before the markers were
			// checked for existence.
			{"usage-history-note alone", usageNoteMarkerMissing},
			{"usage-series-grid alone", usageNoteMarkerMissing},
			{"neither marker present", usageNoteMarkerMissing},
		} {
			if got := usageNotePlacement(tc.island); got != tc.want {
				t.Errorf("usageNotePlacement(%q) = %s, want %s", tc.island, got, tc.want)
			}
		}
	})
}
