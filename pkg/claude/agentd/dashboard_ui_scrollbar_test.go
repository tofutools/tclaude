package agentd

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestDashboardScrollbarTokenContrast holds the house scrollbar (ui.css, see
// jstest/ui-scrollbars.test.mjs) to WCAG non-text contrast: in every skin the
// thumb keeps 3:1 in each state against the lightest pane that commonly
// scrolls there (the thumb is the lighter colour, so that is the hard case),
// and brightens from rest to hover to drag.
func TestDashboardScrollbarTokenContrast(t *testing.T) {
	css := dashboardAssetFile(t, "ui.css")
	for _, skin := range []struct{ name, selector, pane string }{
		{name: "default", selector: ":root", pane: "#21262d"},
		{name: "wizard", selector: ":root:has(> body.wizard)", pane: "#241b3d"},
		{name: "slop", selector: ":root:has(> body.slop)", pane: "#2a0a0a"},
	} {
		rule := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(skin.selector) + ` \{([^}]*--ui-scroll-thumb:[^}]*)\}`).FindStringSubmatch(css)
		if rule == nil {
			t.Fatalf("ui.css has no %s scrollbar tokens", skin.name)
		}
		tokens := map[string]string{}
		for _, m := range regexp.MustCompile(`--ui-scroll-(thumb|thumb-hover|thumb-active): (#[0-9a-f]{6}|var\(--[\w-]+\));`).FindAllStringSubmatch(rule[1], -1) {
			tokens[m[1]] = resolveCSSVar(t, css, m[2])
		}
		if len(tokens) != 3 {
			t.Fatalf("%s skin sets %d scrollbar tokens, want 3", skin.name, len(tokens))
		}
		rest := cssContrastRatio(t, tokens["thumb"], skin.pane)
		hover := cssContrastRatio(t, tokens["thumb-hover"], skin.pane)
		active := cssContrastRatio(t, tokens["thumb-active"], skin.pane)
		for state, ratio := range map[string]float64{"rest": rest, "hover": hover, "active": active} {
			if ratio < 3 {
				t.Errorf("%s %s thumb contrast is %.2f:1, want at least 3:1", skin.name, state, ratio)
			}
		}
		if !(rest < hover && hover < active) {
			t.Errorf("%s thumb must brighten rest < hover < active; got %.2f < %.2f < %.2f", skin.name, rest, hover, active)
		}
	}
	if !strings.Contains(css, "--ui-scroll-thumb") {
		t.Fatal("ui.css lost the scrollbar tokens")
	}
}

func cssContrastRatio(t *testing.T, foreground, background string) float64 {
	t.Helper()
	luminance := func(value string) float64 {
		value = strings.TrimPrefix(value, "#")
		if len(value) != 6 {
			t.Fatalf("invalid CSS hex color %q", value)
		}
		channels := make([]float64, 3)
		for i := range channels {
			parsed, err := strconv.ParseUint(value[i*2:i*2+2], 16, 8)
			if err != nil {
				t.Fatalf("invalid CSS hex color %q: %v", value, err)
			}
			channel := float64(parsed) / 255
			if channel <= 0.04045 {
				channels[i] = channel / 12.92
			} else {
				channels[i] = math.Pow((channel+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*channels[0] + 0.7152*channels[1] + 0.0722*channels[2]
	}
	a, b := luminance(foreground), luminance(background)
	return (math.Max(a, b) + 0.05) / (math.Min(a, b) + 0.05)
}

// resolveCSSVar returns value itself when it is a colour, or the colour a
// var(--name) reference is defined as in css (the wizard gold is a token).
func resolveCSSVar(t *testing.T, css, value string) string {
	t.Helper()
	ref := regexp.MustCompile(`^var\((--[\w-]+)\)$`).FindStringSubmatch(value)
	if ref == nil {
		return value
	}
	def := regexp.MustCompile(regexp.QuoteMeta(ref[1]) + `: (#[0-9a-f]{6});`).FindStringSubmatch(css)
	if def == nil {
		t.Fatalf("ui.css does not define %s as a colour", ref[1])
	}
	return def[1]
}
