package agentd

import (
	"strings"
	"testing"
)

// The retractable right-side PALETTE DOCK (JOH-374, js/dock.js) lists the
// spawn-profile / group-template / role registries as future drag SOURCES on
// the dashboard's right edge — this ticket is the panel shell (drag lands in
// the follow-up tickets). Like the other dashboard render guards this test
// pins the wiring across HTML / CSS / JS by string-searching the embedded
// source rather than running the JS, so a rename in one file that silently
// breaks the dock in the browser fails at `go test ./...` instead.
func TestDashboardHTML_DockPalette(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}
	mustNot := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source unexpectedly contains %q (%s)", needle, why)
		}
	}

	// Markup: the shell + edge toggle are static (they survive the 2s morph);
	// only #dock-body's inner sections get reconciled.
	must(`id="agent-dock"`, "the dock shell exists")
	must(`id="dock-toggle"`, "the edge toggle exists")
	must(`id="dock-body"`, "the morph-target body exists")
	// The head title uses the established span-pair vocab idiom (both modes).
	must(`<span class="tpl-word-regular">🧰 Palette</span><span class="tpl-word-wizard">🧰 Grimoire</span>`,
		"the dock head carries both vocab modes")

	// The dock is NAMED `dock`, not `palette`, because js/palette.js is the
	// Ctrl/Cmd-K command palette — guard against a regression that reuses the
	// command palette's ids/classes for the dock shell.
	mustNot(`id="palette-body"`, "the dock must not collide with the command palette's namespace")

	// CSS: the shell, the collapse-reclaims-space rule, and the collapsed
	// slide-off. The wizard skin must stay SCOPED under #agent-dock (the
	// anti-pin invariant — no unscoped body.wizard widening from this feature).
	must("#agent-dock {", "the dock shell has a CSS rule")
	must("body.dock-open { padding-right: var(--dock-w); }",
		"an open dock reflows the page to reclaim its width")
	must("body:not(.dock-open) #agent-dock { transform: translateX(100%); }",
		"a collapsed dock slides off-screen")
	must("body.wizard #agent-dock .dock-card {", "the wizard skin is scoped under #agent-dock")

	// JS: the module is defined, its two entry points exported, and both are
	// wired from boot / the poll.
	must("export function bindDock(", "dock.js exports its binder")
	must("export function renderDock(", "dock.js exports its poll renderer")
	must("bindDock();", "dashboard.js boot wires the dock")
	must("renderDock();", "refresh.js paints the dock each poll")
	// The open/collapsed state persists server-side via dashPrefs, NOT
	// localStorage (the random per-start port would reset it).
	must("tclaude.dash.dock.open", "the open state persists via a dashPrefs key")

	// Data-driven sections: the three kinds are instances of one SECTIONS
	// idiom (a fourth item kind slots in by adding an entry). Pin the keys so
	// a section can't silently vanish.
	must("const SECTIONS = [", "the sections are data-driven off one config array")
	must(`key: 'profiles'`, "the profiles section exists")
	must(`key: 'templates'`, "the templates section exists")
	must(`key: 'roles'`, "the roles section exists")
	// Cards advertise themselves as future drag sources carrying their payload.
	must(`data-dock-kind="`, "cards carry their kind for the follow-up DnD tickets")
	must(`data-dock-name="`, "cards carry their name for the follow-up DnD tickets")

	// The dock reads its data off the live snapshot (the profile + role
	// registries now ride the poll, templates already did) — see
	// TestDockSnapshot_CarriesProfilesAndRoles for the server-side keys.
	must("snap.profiles", "the profiles section reads off the snapshot")
	must("snap.templates", "the templates section reads off the snapshot")
	must("snap.roles", "the roles section reads off the snapshot")
}
