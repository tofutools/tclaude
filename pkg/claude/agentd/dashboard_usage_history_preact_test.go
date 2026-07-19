package agentd

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestDashboardUsageHistoryPreactBoundary(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		return string(data)
	}
	state := read("js/usage-history-state.js")
	for _, forbidden := range []string{"document", "querySelector", "fetch("} {
		if strings.Contains(state, forbidden) {
			t.Errorf("Usage state contains forbidden DOM/fetch knowledge %q", forbidden)
		}
	}
	chart := read("js/usage-history-chart.js")
	for _, needle := range []string{
		"<polyline", "usage-observed-line", "usage-forecast-line", "usage-reset-mark",
		"usageAxisTicks(start, horizon)", "usage-forecast-hit-target", "usage-scheduled-reset",
		"usage-marker-hit-target", "usage-chart-tooltip",
	} {
		if !strings.Contains(chart, needle) {
			t.Errorf("Usage line chart missing %q", needle)
		}
	}
	if strings.Contains(chart, "previous.pct - point.pct") {
		t.Error("Usage chart infers reset boundaries after downsampling instead of using server markers")
	}
	for _, needle := range []string{
		`data-tab="usage"`, `<div id="usage-root"></div>`, "mountUsageHistoryFeature(),",
		"name: 'usage'", "/api/usage-history?hours=", "Forecasts are per provider × quota window",
		"body.hide-usage-tab nav [data-tab=\"usage\"]",
		"if (name === 'seven_day_sonnet') return '7 day Sonnet';",
		"sampledPoints(points, 240)",
		"headline: 'Prediction paused'",
		"USAGE_LOOKAHEAD_SPANS",
		"Look ahead",
		"`History range, ${scope}`",
		"`Forecast lookahead, ${scope}`",
		"aria-pressed=",
		"usage-card-controls",
		"&spans=",
		"tclaude.dash.usage.seriesSpans",
		// A provider's quota windows share one centred row, capped at the
		// per-card width times that row's card count.
		"groupSeriesByProvider(current.series)",
		"usage-provider-row",
		"--usage-cols:${row.series.length}",
		// The row-width variables live on the host, so the island-load-failure
		// banner (rendered into #usage-root, outside the island) inherits them.
		"#usage-root { --usage-card-max: 1100px; --usage-card-gap: 14px; }",
		// Provider rows are centred. Safe as a standalone `margin-inline` only
		// because no later rule touches .usage-provider-row's margin; anything
		// that grows a competing shorthand must move this to `margin: 0 auto`.
		"margin-inline: auto; width: 100%;",
		// The legend renders per card, under its chart — side-by-side graphs
		// have no line of sight to one shared legend. It is aria-hidden: a
		// visual key to SVG stroke styles is meaningless to a screen reader,
		// and the chart itself carries the accessible name.
		"function UsageChartLegend()",
		`<div class="usage-chart-legend" aria-hidden="true">`,
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("Usage Preact wiring missing %q", needle)
		}
	}
	if strings.Contains(chart, "point.source") {
		t.Error("Usage point tooltip exposes internal sample source")
	}
	// The tab must open on the graphs. Nothing explanatory sits above the grid:
	// the old top-of-tab controls bar is gone entirely, and the note is a
	// footnote after the grid rather than a header before it.
	island := read("js/usage-history-island.js")
	// Matched in markup context: a bare substring would also hit any comment
	// mentioning the removed bar, and this file is heavily commented.
	if strings.Contains(island, `class="usage-history-controls"`) {
		t.Error("Usage tab reintroduced the top-of-tab controls bar above the graphs")
	}
	switch placement := usageNotePlacement(island); placement {
	case usageNoteMarkerMissing:
		t.Error("Usage island is missing the note or grid marker, so its placement cannot be checked")
	case usageNoteAboveGrid:
		t.Error("Usage explanatory note renders above the graph grid instead of below it")
	}
	// The note is a full-width footnote: capping or centring it would line it
	// up with the rows above, which is what moving it out of the header undid.
	for _, rule := range usageCSSRules(t, read("dashboard.css"), ".usage-history-note") {
		if strings.Contains(rule, "max-width") || usageMarginIsAuto(rule) {
			t.Errorf("Usage note should be uncapped and left-aligned, got rule: %s", rule)
		}
	}
}

// Placement verdicts for the explanatory note relative to the graph grid.
const (
	usageNoteBelowGrid     = "below-grid"
	usageNoteAboveGrid     = "above-grid"
	usageNoteMarkerMissing = "marker-missing"
)

// usageNotePlacement reports where the explanatory note sits relative to the
// graph grid in the island source. Both markers must be present for their order
// to mean anything: strings.Index returns -1 for absent, and a present note is
// never < -1, so a blind comparison would silently pass whenever either class
// is renamed or deleted — reporting "below" for a grid that isn't there.
func usageNotePlacement(island string) string {
	noteAt, gridAt := strings.Index(island, "usage-history-note"), strings.Index(island, "usage-series-grid")
	switch {
	case noteAt < 0 || gridAt < 0:
		return usageNoteMarkerMissing
	case noteAt < gridAt:
		return usageNoteAboveGrid
	default:
		return usageNoteBelowGrid
	}
}

// usageCSSRules returns the declaration block of every rule whose selector list
// mentions `selector`, innermost-first-to-last in source order, including rules
// nested in @media. All of them matter: dashboard.css is flat, so the rule that
// wins is the *last* one — a helper that returned only the first would be blind
// to exactly the later-override hazard these assertions guard against.
func usageCSSRules(t *testing.T, css, selector string) []string {
	t.Helper()
	// Each match is one innermost `selectors { declarations }`; neither group
	// can span a brace, so an @media wrapper is skipped and the rules inside it
	// are matched individually.
	var rules []string
	for _, m := range regexp.MustCompile(`([^{}]*)\{([^{}]*)\}`).FindAllStringSubmatch(css, -1) {
		if strings.Contains(m[1], selector) {
			rules = append(rules, m[2])
		}
	}
	if len(rules) == 0 {
		t.Fatalf("no %q rule in dashboard.css", selector)
	}
	return rules
}

// usageMarginIsAuto reports whether a declaration block centres itself via an
// `auto` inline margin. It inspects only margin declarations: a bare "auto"
// substring would also fire on `overflow-x: auto`, `height: auto` and friends.
func usageMarginIsAuto(rule string) bool {
	for decl := range strings.SplitSeq(rule, ";") {
		prop, value, ok := strings.Cut(decl, ":")
		if !ok {
			continue
		}
		prop = strings.TrimSpace(prop)
		if prop == "margin" || strings.HasPrefix(prop, "margin-") {
			if strings.Contains(value, "auto") {
				return true
			}
		}
	}
	return false
}
