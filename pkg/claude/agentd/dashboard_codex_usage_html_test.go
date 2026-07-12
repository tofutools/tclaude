package agentd

import (
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDashboardHTML_CodexUsageWired guards the top-bar Codex usage readout —
// the labelled "Claude" / "Codex" two-line layout that renders when
// snapshot.usage.codex is present. The pieces span render.js (builds the
// lines off usage.codex) and dashboard.css (the .multiline / .uline / .usrc
// layout); a rename in one silently breaks the feature in the browser, and
// the repo has no JS test runner, so this asserts on the embedded asset
// bundle at `go test ./...`.
func TestDashboardHTML_CodexUsageWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// render.js: the readout reads the nested codex object and renders the
	// labelled lines through the shared helpers.
	must("function subscriptionWindowsHTML(src, hideMissing)", "shared per-source window builder is defined")
	must("function usageLineHTML(label, tokens)", "labelled-line builder is defined")
	must("u && u.codex", "renderUsage reads the codex sub-object off the snapshot")
	must("usageLineHTML('Claude:'", "the Claude line is labelled")
	must("usageLineHTML('Codex:'", "the Codex line is labelled")
	must("classList.add('multiline')", "the two-line layout toggles the multiline class")

	// dashboard.css: the multiline layout + the right-aligned source label
	// column that stacks the colons.
	must("#usage.multiline", "multiline stacks the readout vertically")
	must("#usage .usrc", "the source label column is styled")
}

// TestDashboardHTML_CodexUsageColumnAlignment guards the two-line readout's
// column alignment: a monospace block where each field reserves a worst-case
// `ch` width, so the Claude/Codex rows line up field-for-field and the layout
// doesn't shift as a countdown ticks down or a percent crosses 99→100. CSS
// only, so it lives with the other asset-wiring guards.
func TestDashboardHTML_CodexUsageColumnAlignment(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// The block is monospace so `ch` widths are predictable, and the source
	// label is inline-block (a bare inline span ignores min-width/text-align).
	must("#usage.multiline {", "the multiline container is styled as a block")
	must("display: inline-block", "fields reserve width via inline-block")
	// Worst-case reserved widths: source label, percent, and reset hint.
	// The reset hint is 8ch (not 7ch): the 7-day/weekly window in its final
	// sub-24h stretch renders "(23h59m)" = 8 chars, and a 7ch reserve let it
	// overflow and desync the right-aligned rows (see
	// TestDashboardCSS_UsageRemColumnFitsWorstCaseRemaining). 7ch survives as
	// the source-label column ("Claude:").
	must("min-width: 7ch;", "the source-label column reserves \"Claude:\"")
	must("min-width: 4ch;", "the percent column reserves up to \"100%\"")
	must("min-width: 8ch;", "the reset-hint column reserves its worst case \"(23h59m)\"")
	must("#usage.multiline .upct", "the percent column is fixed-width + right-aligned")
	must("#usage.multiline .urem", "the reset-hint column is fixed-width")

	// render.js must always emit the .urem span — even for a window with no
	// remaining-time text (a harness idle long enough that the window has
	// reset) — so its fixed min-width still reserves the column and the rows
	// stay aligned. Dropping the span collapsed the column and slid the
	// following windows left.
	must("const rem = '<span class=\"urem\">' + remText + '</span>'", "the reset-hint column is always emitted, even when empty")
	// And it must NOT reintroduce a leading space before .urem (it would
	// become a stray flex item and break the monospace column widths); the
	// parens wrap only when there is remaining text.
	must("win.remaining ? '(' + esc(win.remaining) + ')' : ''", "no leading space; parens only when there is remaining text")
}

// TestDashboardHTML_UsageAlwaysReservesBothWindows guards the two-line
// readout's alignment rule: both window slots are emitted even when a source
// omits one. Codex's omitted slot is hidden rather than rendered as a
// misleading 0%, but visibility:hidden preserves its geometry so 7d remains
// under 7d. Claude retains its visible zero placeholder behavior.
func TestDashboardHTML_UsageAlwaysReservesBothWindows(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}
	reject := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets still carry %q (%s)", needle, why)
		}
	}
	must("usageWindowHTML('5h', src.five_hour || zero, hideMissing && !src.five_hour)", "the 5h slot is always emitted and can be hidden when absent")
	must("usageWindowHTML('7d', src.seven_day || zero, hideMissing && !src.seven_day)", "the 7d slot is always emitted and can be hidden when absent")
	must("subscriptionWindowsHTML(u && u.codex, true)", "Codex hides missing limit placeholders")
	must("#usage .uw.umissing { visibility: hidden; }", "missing Codex windows retain their alignment geometry")
	must("aria-hidden=\"true\"", "hidden placeholders are omitted from the accessibility tree")
	// The old conditional guards dropped a window when its data was absent —
	// the exact shape that let the two rows carry different window counts.
	reject("if (src.five_hour) wins.push(usageWindowHTML('5h'", "the conditional 5h guard is gone")
	reject("if (src.seven_day) wins.push(usageWindowHTML('7d'", "the conditional 7d guard is gone")
}

// TestDashboardCSS_UsageRemColumnFitsWorstCaseRemaining ties the reset-hint
// column's reserved width to formatRemaining's actual worst-case output, so
// the two halves can't drift apart again. The two-line readout reserves a
// fixed `ch` width per field and right-aligns the rows; if a remaining-time
// string is wider than its column it overflows, makes its row wider than the
// other, and — because the block hangs off the right edge — slides every field
// out of alignment. The widest hint is the 7-day/weekly window in its final
// sub-24h stretch: formatRemaining's "%dh%dm" branch yields "23h59m", which
// the renderer wraps to "(23h59m)" = 8 chars. This test recomputes that worst
// case from formatRemaining itself and asserts the CSS column is at least that
// wide — so a future formatter change that widens the hint fails here instead
// of silently breaking the browser layout.
func TestDashboardCSS_UsageRemColumnFitsWorstCaseRemaining(t *testing.T) {
	now := time.Now()
	// Representative resets covering every formatRemaining branch and the
	// digit-width extremes a bounded (≤7-day) window can reach.
	cases := []time.Time{
		now.Add(30*time.Minute + 30*time.Second),                // "30m"     -> "(30m)"     5
		now.Add(2*time.Hour + 35*time.Minute + 30*time.Second),  // "2h35m"   -> "(2h35m)"   7
		now.Add(23*time.Hour + 59*time.Minute + 30*time.Second), // "23h59m" -> "(23h59m)"  8  (worst)
		now.Add(6*24*time.Hour + 23*time.Hour + 30*time.Minute), // "6d23h"  -> "(6d23h)"   7
		now.Add(-time.Hour), // past     -> "(reset)"   7
		{},                  // zero     -> ""          0
	}
	worst := 0
	var worstStr string
	for _, resetsAt := range cases {
		s := formatRemaining(resetsAt)
		w := 0
		if s != "" { // the renderer wraps a non-empty hint in parens
			w = len("(" + s + ")")
		}
		if w > worst {
			worst, worstStr = w, "("+s+")"
		}
	}
	// Sanity-pin the worst case so a formatRemaining change that quietly drops
	// "23h59m" (and with it this guard's teeth) is itself visible.
	if worst != 8 || worstStr != "(23h59m)" {
		t.Fatalf("unexpected worst-case remaining hint: got %d chars %q, want 8 chars \"(23h59m)\" — "+
			"if formatRemaining legitimately changed, update this expectation and the .urem reserve together", worst, worstStr)
	}

	cssBytes, err := fs.ReadFile(dashboardAssetsFS, "dashboard.css")
	if err != nil {
		t.Fatalf("reading embedded dashboard.css: %v", err)
	}
	remCh := cssMinWidthCh(t, string(cssBytes), "#usage.multiline .urem")
	if remCh < worst {
		t.Errorf("the reset-hint column reserves %dch but the worst-case hint %q is %d chars wide — "+
			"it overflows and desyncs the right-aligned two-line readout; widen #usage.multiline .urem",
			remCh, worstStr, worst)
	}
}

// cssMinWidthCh extracts the `min-width: Nch` declared on the rule for
// selector, returning N. It scans forward from the selector to its first
// min-width declaration (each #usage column rule is a one-line block:
// "selector { ... min-width: Nch; ... }"), so the surrounding rationale
// comments — which never contain "min-width:" — are ignored.
func cssMinWidthCh(t *testing.T, css, selector string) int {
	t.Helper()
	idx := strings.Index(css, selector+" ")
	if idx < 0 {
		idx = strings.Index(css, selector+"{")
	}
	if idx < 0 {
		t.Fatalf("selector %q not found in dashboard.css", selector)
	}
	m := regexp.MustCompile(`min-width:\s*(\d+)ch`).FindStringSubmatch(css[idx:])
	if m == nil {
		t.Fatalf("no `min-width: Nch` found for selector %q", selector)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parsing min-width %q for %q: %v", m[1], selector, err)
	}
	return n
}
