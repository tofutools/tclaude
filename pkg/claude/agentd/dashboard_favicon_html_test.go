package agentd

import (
	"strings"
	"testing"
)

// The dashboard favicon is an inline SVG data URI in dashboard.html's
// <head> — no Go route, no embedded asset. There is no JS test runner
// in the repo, so this guard pins the markup: a future <head> edit
// can't silently drop the icon, and the 🤝 emoji (the brief's explicit
// ask) can't be swapped out unnoticed.
func TestDashboardHTML_FaviconHandshake(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard.html missing %q (%s)", needle, why)
		}
	}

	// A favicon link must exist.
	must(`<link rel="icon"`, "the page declares a favicon")

	// It must be a self-contained inline SVG data URI — no extra route
	// or asset fetch, so it works behind the dashboard auth cookie.
	must(`href='data:image/svg+xml,`, "favicon is an inline SVG data URI")

	// The 🤝 handshake emoji is the icon itself — the literal codepoint
	// the brief asked for, carried verbatim in the UTF-8 page.
	must("\U0001F91D", "favicon uses the 🤝 handshake emoji")
	must("</text></svg>'>", "the inline SVG is well-formed and closed")
}
