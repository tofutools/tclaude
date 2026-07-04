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
	// JOH-388 req 2: two more discoverable show/hide controls beyond the edge
	// tab — a top-bar toggle next to the windows/power controls, and an in-dock
	// collapse affordance in the (now title-less, req 6) dock header.
	must(`id="dock-toggle-top"`, "the top-bar dock toggle exists (discoverable show/hide)")
	must(`id="dock-collapse"`, "the in-dock collapse affordance exists")
	// The "Palette"/"Grimoire" vocab span-pair moved from the dropped head title
	// (req 6) onto the top-bar toggle — same established span-pair idiom, both
	// modes, so the CSS .tpl-word-* swap picks the voice per theme.
	must(`<span class="tpl-word-regular">🧰 Palette</span><span class="tpl-word-wizard">🧰 Grimoire</span>`,
		"the top-bar dock toggle carries both vocab modes")

	// The dock is NAMED `dock`, not `palette`, because js/palette.js is the
	// Ctrl/Cmd-K command palette — guard against a regression that reuses the
	// command palette's ids/classes for the dock shell.
	mustNot(`id="palette-body"`, "the dock must not collide with the command palette's namespace")

	// CSS: the shell, the collapse-reclaims-space rule, and the collapsed
	// slide-off. The wizard skin must stay SCOPED under #agent-dock (the
	// anti-pin invariant — no unscoped body.wizard widening from this feature).
	must("#agent-dock {", "the dock shell has a CSS rule")
	// JOH-388 req 3: an open dock reserves its width AND makes BODY the
	// horizontal scroll container, so the padding-right becomes real scroll
	// clearance and wide content can be scrolled clear of the fixed dock rather
	// than sliding underneath it (padding-inline-end counts in a scroll
	// container's scrollable overflow). This pin was swapped from the pre-rework
	// bare `padding-right` rule when the overflow-x mechanism landed.
	must("body.dock-open { padding-right: var(--dock-w); overflow-x: auto; }",
		"an open dock reserves its width AND becomes the h-scroll container (content clears the dock)")
	// JOH-388 req 1: the dock rail spans only the content area (top tracks the
	// chrome via --dock-top, bottom pinned to the footer) instead of covering
	// the header/nav controls at top:0.
	must("top: var(--dock-top); right: 0; bottom: var(--footer-h);",
		"the dock rail spans the content area (below the top bar, above the footer)")
	must("body:not(.dock-open) #agent-dock { transform: translateX(100%); }",
		"a collapsed dock slides off-screen")
	must("body.dock-anim #agent-dock { transition: transform", "the slide is gated behind .dock-anim (no flash-in on load)")
	must("body.wizard #agent-dock .dock-card {", "the wizard skin is scoped under #agent-dock")
	// JOH-388 req 5: each category is a collapsible <details>; its per-section
	// fold persists via dashPrefs and the disclosure chevron flips on [open].
	must(".dock-section[open] > .dock-section-head .dock-section-chevron", "the section chevron flips with the <details> open state")
	must("tclaude.dash.dock.section.", "each section's collapse persists via a dashPrefs key")
	must("classList.add('dock-anim')", "dock.js enables the slide only after the initial paint")

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
	// Cards carry their payload for the drag wiring (dock-dnd.js reads these off
	// dragstart; see TestDashboardHTML_DockDnd for the 2/4 drag behaviour).
	must(`data-dock-kind="`, "cards carry their kind for the DnD wiring")
	must(`data-dock-name="`, "cards carry their name for the DnD wiring")

	// The dock reads its data off the live snapshot (the profile + role
	// registries now ride the poll, templates already did) — see
	// TestDockSnapshot_CarriesProfilesAndRoles for the server-side keys.
	must("snap.profiles", "the profiles section reads off the snapshot")
	must("snap.templates", "the templates section reads off the snapshot")
	must("snap.roles", "the roles section reads off the snapshot")
}
