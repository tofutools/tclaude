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

	// Markup: the shell + edge toggle are static; #dock-body is exclusively
	// owned by the keyed Preact island.
	must(`id="agent-dock"`, "the dock shell exists")
	must(`id="dock-toggle"`, "the edge toggle exists")
	must(`id="dock-body"`, "the Preact island host exists")
	must("mountDockFeature()", "dashboard boot mounts the dock island")
	must("name: 'dock'", "the dock has an island descriptor")
	// The in-dock collapse affordance in the (title-less, req 6) dock header.
	must(`id="dock-collapse"`, "the in-dock collapse affordance exists")
	// JOH-390 item 7: the top-bar "🧰 Palette"/"🧰 Grimoire" toggle (JOH-388 req 2)
	// was REMOVED per operator request — a duplicate reopen control. The final
	// show/hide surface is TWO controls (edge tab + in-dock collapse). The button,
	// its id and its vocab span-pair must all be gone (the Palette/Grimoire vocab
	// simply died with the button — the section headings carry the meaning, req 6).
	mustNot(`id="dock-toggle-top"`, "the removed top-bar dock toggle leaves no id behind (JOH-390 item 7)")
	mustNot(`🧰 Palette`, "the removed top-bar toggle's vocab span-pair is gone (JOH-390 item 7)")

	// The dock is NAMED `dock`, not `palette`, because js/palette.js is the
	// Ctrl/Cmd-K command palette — guard against a regression that reuses the
	// command palette's ids/classes for the dock shell.
	mustNot(`id="palette-body"`, "the dock must not collide with the command palette's namespace")

	// CSS: the shell, the collapse-reclaims-space rule, and the collapsed
	// slide-off. The wizard skin must stay SCOPED under #agent-dock (the
	// anti-pin invariant — no unscoped body.wizard widening from this feature).
	must("#agent-dock {", "the dock shell has a CSS rule")
	// An open dock reserves its width so in-flow content lays out clear of the
	// rail (unchanged from the pre-rework rule — byte-identical).
	must("body.dock-open { padding-right: var(--dock-w); }",
		"an open dock reflows the page to reclaim its width")
	// JOH-388 req 3: the reservation alone doesn't help horizontally-scrolling
	// content (a wide table nested in <main> overflows past body's padding-right
	// and slides under the fixed dock; Chrome adds a scroll container's
	// padding-inline-end only after DIRECT children, not deep-descendant
	// overflow). #dock-hscroll-pad is the fix — a scroll-content spacer that
	// hscroll.js parks --dock-w past the widest content so it can be scrolled
	// clear of the dock. Verified end-to-end by the dashsnap dock-wide-scroll
	// state (self-checking: it throws if the tail doesn't clear).
	must("#dock-hscroll-pad {", "the horizontal-scroll clearance spacer has a CSS rule")
	must(`id="dock-hscroll-pad"`, "the clearance spacer exists in the markup")
	must("pad.classList.add('on')", "hscroll.js parks the clearance spacer past the content when the dock overflows")
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
	// setDockOpen is exported so the Ctrl/Cmd-K command palette can flip the dock
	// through the same one source of truth the edge tab + in-dock collapse button
	// use (see TestDashboardHTML_CommandPalette for the palette-side wiring). Both
	// of those controls now route through the shared setDockOpen too.
	must("export function setDockOpen(", "dock.js exports the shared open/collapse mutation for the palette")
	must("const toggleDock = () => setDockOpen(", "the edge tab + in-dock collapse route through the shared setDockOpen")
	// The open/collapsed state persists server-side via dashPrefs, NOT
	// localStorage (the random per-start port would reset it).
	must("tclaude.dash.dock.open", "the open state persists via a dashPrefs key")

	// Data-driven sections: the three kinds are instances of one dockSections
	// idiom (a fourth item kind slots in by adding an entry). Pin the keys so
	// a section can't silently vanish.
	must("export const dockSections = Object.freeze([", "the sections are data-driven off one config array")
	must(`key: 'profiles'`, "the profiles section exists")
	must(`key: 'templates'`, "the templates section exists")
	must(`key: 'roles'`, "the roles section exists")
	// Cards carry their payload for the drag wiring (dock-dnd.js reads these off
	// dragstart; see TestDashboardHTML_DockDnd for the 2/4 drag behaviour).
	must("data-dock-kind=${section.key}", "cards carry their kind for the DnD wiring")
	must("data-dock-name=${name}", "cards carry their name for the DnD wiring")
	// Profile cards keep their compact truncated chips in place while a floating
	// tooltip shows the complete wrapped list on mouse hover or keyboard focus.
	// It opens left of the card, leaving the item column unobstructed, and native
	// titles are suppressed so the browser cannot add a second tooltip.
	must("fullChips: (p) => [", "profiles provide an untruncated chip list")
	must("profileDetailChips(p).map", "tooltips enumerate the complete stored profile shape")
	must(`role="region"`, "the card renders complete profile details as an accessible region")
	must(`tabIndex="0"`, "overflowing profile details can receive focus and scroll from the keyboard")
	must("detailsDescriptionID", "the action button and focusable region describe every full-detail chip")
	must("onMouseEnter=${hasDetails ? enterCard : null}", "hover reveals the complete tooltip")
	must("onFocusIn=${hasDetails ? showDetails : null}", "keyboard focus reveals the complete tooltip")
	must("window.addEventListener('resize', positionDetails)", "open details follow viewport geometry changes")
	must("clipHost?.addEventListener('scroll', positionDetails", "open details follow dock scrolling")
	must(".dock-card-details.open", "the complete details float without resizing the card")
	must("const left = Math.max(8, cardRect.left - cardRect.width)", "details open to the left of the hovered card")
	must("width: Math.max(0, cardRect.left - left)", "details never overlap the profile-item column")
	must("detailsPosition?.width", "details remeasure their height after a viewport-width change")
	must("title=${hasDetails ? null : name}", "rich details replace the card's native title tooltip")
	must("title=${hasDetails ? null : gripTitle}", "profile drag grips do not add a second native tooltip")
	must("title=${hasDetails ? null : 'More actions'}", "profile action buttons do not add a second native tooltip")
	must("onDragStart=${section.drag ? () => {", "starting a card drag closes rich profile details")
	must("detailsOpen && !menuOpen && !dragging", "rich details remain hidden throughout a card drag")
	must("onDragLeave=${section.drag ? (event) => {", "native drag events track whether the pointer leaves its source card")
	must("const endedOverCard = hit ? event.currentTarget.contains(hit) : hoveringRef.current", "dragend restores details only over the source card")

	// The dock reads its data off the live snapshot (the profile + role
	// registries now ride the poll, templates already did) — see
	// TestDockSnapshot_CarriesProfilesAndRoles for the server-side keys.
	must("snap.profiles", "the profiles section reads off the snapshot")
	must("snap.templates", "the templates section reads off the snapshot")
	must("snap.roles", "the roles section reads off the snapshot")

	// --- JOH-390 dock polish r2 -------------------------------------------

	// Item 1: the footer spans the FULL viewport width even with the dock open.
	// The dock's bottom edge is pinned to var(--footer-h) — the same variable as
	// the footer's height — so it never occupies the footer's band; squeezing the
	// footer to viewport-minus-dock cramped/clipped the base-URL line. Guard the
	// squeeze rule stays gone.
	mustNot("body.dock-open footer {",
		"the footer is not squeezed to viewport-minus-dock (JOH-390 item 1 — it spans full width, clear below the dock)")

	// Item 4: the groups-toolbar globals ("+ new group", the ⚙ cog, the 🧠
	// default-profile chip) re-home into the open dock's head — two static
	// containers (outside the #dock-body Preact ownership boundary) that
	// dock.js's syncDockActions moves the live nodes into on open / back to the
	// toolbar on collapse.
	must(`id="dock-actions-primary"`, "the dock head hosts the re-homed new-group + cog row")
	must(`id="dock-actions-profile"`, "the dock head hosts the re-homed default-profile row")
	must("function syncDockActions(", "dock.js moves the toolbar globals in/out of the dock head")
	must(".dock-actions-profile:empty { display: none; }",
		"the re-homed profile row collapses when its chip is back in the toolbar")

	// Item 5: the profiles section carries its FULL name in the dock (operator
	// request); templates + roles keep their short headings.
	must(`wizWord('Agent profiles', 'Familiar patterns')`, "the profiles section heading is spelled out (JOH-390 item 5)")

	// Item 6: the template card's per-item ⚙ deep-links into THAT template's
	// editor (like profiles/roles), not the whole-kind manager.
	must("openTemplateEditor(t)", "the template card ⚙ opens the item's editor (JOH-390 item 6)")

	// --- Groups-tab-only gate ---------------------------------------------
	// The dock is offered ONLY on the Groups tab: its cards drag onto GROUP
	// rows, so it's meaningless on Jobs / Access / Config / Costs / … . dock.js
	// reads the active tab off the pane's `.active` class and mirrors it as a
	// body.dock-tab class; CSS removes the whole shell (panel + edge toggle)
	// off-tab, and the effective open state is forced off so no page space is
	// reserved. One observer over the Groups pane's class re-evaluates on every
	// tab switch, so every switch site (bindTabs, the auto-hide redirects,
	// showAccessTab, the command palette, keyboard cycling) is covered.
	must("body:not(.dock-tab) #agent-dock { display: none; }",
		"the dock shell is hidden entirely off the Groups tab")
	must("function isDockTab(", "dock.js reads whether Groups is the active tab")
	must("classList.toggle('dock-tab'", "dock.js mirrors the tab availability as a body class")
	// The observer keys off the Groups pane's class — the source of truth every
	// tab-switch site writes — so a new switch path is covered without a hook.
	must(`getElementById('tab-groups')`, "dock.js gates the dock on the Groups pane's active state")
}
