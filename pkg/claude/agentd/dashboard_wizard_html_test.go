package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardHTML_WizardTheme pins the wizard ("it's wizard time") re-skin's
// client-side wiring. Like the slop guards it is purely front-end, so we
// string-search the embedded source (dashboard.html + dashboard.css + every
// js/*.js) rather than running the JS — a dropped bootstrap call, a renamed
// export or a refactored hotkey clause would otherwise break the theme
// silently in the browser.
func TestDashboardHTML_WizardTheme(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Public surface (slop.js owns the theme machinery, including wizard).
	must("export function toggleWizard(", "slop.js exports the wizard toggle")
	must("export function isWizardActive(", "slop.js exports the wizard-active check")
	must("export function cycleTheme(", "slop.js exports the header-icon cycle")
	must("export function bindWizardHotkey", "slop.js exports the wizard hotkey binder")

	// URL param → body class, mutually exclusive with slop.
	must("params.get('wizard') === '1'", "applySlopThemeIfRequested applies ?wizard=1")
	must("document.body.classList.add('wizard')", "?wizard=1 adds the body.wizard class")

	// Mutual exclusion is the load-bearing invariant (the two re-skins must
	// never both paint). Pin the class-clears so a refactor that drops one —
	// leaving both body classes settable at once — fails CI rather than the
	// browser. toggleSlop clears wizard, toggleWizard clears slop, cycleTheme
	// clears both before setting the next.
	must("document.body.classList.remove('wizard')", "toggleSlop clears wizard (mutual exclusion)")
	must("document.body.classList.remove('slop')", "toggleWizard clears slop (mutual exclusion)")
	must("classList.remove('slop', 'wizard')", "cycleTheme clears both before setting the next")

	// The header icon cycles through the three themes rather than a 2-state
	// toggle — regular → slop → wizard.
	must("iconSpan.addEventListener('click', cycleTheme)", "the header icon cycles themes")

	// The music features light up in wizard mode too (isVegasActive gates the
	// radio/HUD): the class must be in the OR.
	must("classList.contains('wizard')", "isVegasActive includes wizard mode")

	// Bootstrap wiring: dashboard.js must install the hotkey + the FX binders.
	must("bindWizardHotkey();", "dashboard.js installs the wizard hotkey at bootstrap")
	must("bindWizardCursorTrail();", "dashboard.js installs the wizard cursor trail")
	must("bindWizardCastFx();", "dashboard.js installs the wizard cast FX")
	must("bindWizardStatusWatch();", "dashboard.js installs the wizard status watch")
	must("bindWizardMarquee();", "dashboard.js installs the wizard marquee")
	must("bindWizardSpectacle();", "dashboard.js installs the wizard spectacle (Meteor Swarm)")

	// The +W hotkey identity: physical W key (layout-independent), Shift+Alt,
	// and either Ctrl or Cmd — the wizard twin of the +S slop hotkey. Pin each
	// clause so a refactor can't quietly widen the accident surface.
	must("e.code !== 'KeyW'", "matches the physical W key, not layout-dependent e.key")
	must("toggleWizard();", "the +W hotkey flips wizard mode")

	// The wizard state pill replaces the plain pill in wizard mode: helper
	// exported + called from render.js alongside the slot machine.
	must("wizardPill,", "helpers.js exports the wizard pill")
	must("wizardPill(state, m.online, m.conv_id)", "render.js emits the wizard pill in the state cell")

	// The activity-bot row gets a wizard re-skin too: a third wrapper
	// (.ga-wizard) with fantasy glyphs, emitted alongside regular + slop and
	// CSS-swapped in via body.wizard (same "always emit, theme picks" trick).
	must("export function wizardBotsHTML(", "group-activity.js exports the wizard bot row")
	must("wizardBotsHTML,", "render.js imports the wizard bot row")
	must("body.wizard .ga-wizard", "dashboard.css shows the wizard bot row in wizard mode")
	must("body.wizard .ga-regular { display: none", "wizard mode hides the plain bot row")

	// Command palette offers a wizard theme command.
	must("'Switch to wizard theme'", "the palette offers a wizard theme command")
	must("run: () => toggleWizard(),", "the wizard palette command runs toggleWizard")

	// CSS re-skin hooks.
	must("body.wizard {", "dashboard.css carries the wizard re-skin")
	must("body.wizard nav button[data-tab=\"vegas\"]", "the music tab shows in wizard mode")
	must(".tab-label-wizard", "the music tab has a wizard label variant")
	must("body.wizard #slop-marquee", "the marquee shows in wizard mode")
	must(".wizard-spark", "the wizard cast/trail spark FX are styled")
	must("body.wizard-shake", "the Meteor Swarm screen shake is styled")
}

// TestDashboardHTML_WizardSpawnModal pins the wizard re-skin of the spawn
// dialog ("Summon a new familiar"): the per-theme title copy swap and the
// arcane CSS hooks. Purely front-end like the rest of the wizard theme, so we
// string-search the embedded source rather than running the JS.
func TestDashboardHTML_WizardSpawnModal(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Title copy: a pure-CSS span swap, "Spawn a new agent" → "Summon a new
	// familiar". Both spans must exist in the HTML and the swap rules in CSS.
	must(`<span class="spawn-title-regular">Spawn a new agent</span>`, "the default spawn title span")
	must(`<span class="spawn-title-wizard">Summon a new familiar</span>`, "the wizard spawn title span")
	must("body.wizard #agent-spawn-title .spawn-title-regular", "wizard hides the default title")
	must("body.wizard #agent-spawn-title .spawn-title-wizard", "wizard shows the familiar title")

	// The submit button becomes a "🔮 Summon!" conjuring button (slop's PULL
	// twin) — the ::before glyph + the modal-scoped submit re-skin.
	must(`content: "🔮 Summon!"`, "the submit button reads Summon in wizard mode")
	must("body.wizard #agent-spawn-modal #agent-spawn-submit", "the submit re-skin is scoped to the spawn modal")

	// The whole dialog is re-skinned arcane.
	must("body.wizard #agent-spawn-modal .cron-create-modal", "the spawn dialog surface is re-skinned")
}

// TestDashboardHTML_WizardRetireModal pins the wizard re-skin of the retire
// confirmation ("Banish this familiar?"): the per-theme title copy swap and the
// arcane CSS hooks — the destructive twin of the spawn dialog's Summon re-skin.
// Purely front-end like the rest of the wizard theme, so we string-search the
// embedded source rather than running the JS.
func TestDashboardHTML_WizardRetireModal(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Title copy: a pure-CSS span swap, "Retire this agent?" → "Banish this
	// familiar?". Both spans must exist in the HTML and the swap rules in CSS.
	must(`<span class="retire-title-regular">Retire this agent?</span>`, "the default retire title span")
	must(`<span class="retire-title-wizard">Banish this familiar?</span>`, "the wizard retire title span")
	must("body.wizard #retire-title .retire-title-regular", "wizard hides the default title")
	must("body.wizard #retire-title .retire-title-wizard", "wizard shows the banish title")

	// The confirm button becomes a "🪄 Banish!" lever (Summon's twin) — the
	// ::before glyph + the modal-scoped confirm re-skin.
	must(`content: "🪄 Banish!"`, "the confirm button reads Banish in wizard mode")
	must("body.wizard #retire-modal #retire-ok", "the confirm re-skin is scoped to the retire modal")

	// The whole dialog is re-skinned arcane.
	must("body.wizard #retire-modal .modal", "the retire dialog surface is re-skinned")
}

// TestDashboardHTML_WizardSummonFx pins the wizard spawn-celebration wiring
// ("It's wizard time!"): the wizard twin of slop's spawn jackpot. Like the rest
// of the wizard theme it is purely front-end, so we string-search the embedded
// source rather than running the JS — a dropped call or a renamed export would
// otherwise lose the feature silently in the browser.
func TestDashboardHTML_WizardSummonFx(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The module is embedded — without it modal-spawn.js's import would 404.
	if _, err := fs.ReadFile(dashboardAssetsFS, "js/wizard-fx.js"); err != nil {
		t.Fatalf("embedded js/wizard-fx.js missing: %v", err)
	}

	// Public surface + both sides of the wiring: wizard-fx exports the summon
	// celebration and modal-spawn fires it on a successful spawn, right next to
	// the slop jackpot (the two themes are mutually exclusive, so calling both
	// paints at most one).
	must("export function wizardSummon(", "wizard-fx.js exports the summon celebration modal-spawn.js calls")
	must("wizardSummon();", "modal-spawn.js fires the summon celebration on a successful spawn")

	// The banner text is theme flavour, not just an emoji — pin a couple of the
	// silly spell quotes so a refactor that empties the pool trips here.
	must("wizard time!", "the summon banner carries the headline spell quote")
	must("🔥 Fireball!", "the summon quote pool keeps its silly spells")

	// CSS hooks the JS creates by classname: the width-capped inner text span
	// and the font-clamped summon-banner variant.
	must(".wizard-banner-text", "the summon banner text wrapper is styled")
	must(".wizard-summon-banner", "the summon banner font-clamp variant is styled")
}

// TestDashboardCSS_WizardRetireModalScoped guards that the wizard retire-dialog
// re-skin stays scoped to #retire-modal. The retire modal is a generic .modal
// (unlike the spawn dialog's .cron-create-modal), so an unscoped
// `body.wizard .modal { … }` would repaint every sibling confirm dialog
// (shutdown / delete-agent / power-on), which is not the intent.
func TestDashboardCSS_WizardRetireModalScoped(t *testing.T) {
	if strings.Contains(dashboardAssets, "body.wizard .modal {") {
		t.Error("wizard retire re-skin is unscoped — will repaint shutdown/delete-agent confirms too")
	}
}

// TestDashboardCSS_WizardSpawnModalScoped guards that the wizard spawn-dialog
// re-skin stays scoped to #agent-spawn-modal — an unscoped
// `body.wizard .cron-create-modal { … }` would repaint every sibling modal
// (clone / reincarnate / cron / group create), which is not the intent.
func TestDashboardCSS_WizardSpawnModalScoped(t *testing.T) {
	if strings.Contains(dashboardAssets, "body.wizard .cron-create-modal {") {
		t.Error("wizard spawn re-skin is unscoped — will repaint clone/reincarnate/cron modals too")
	}
}

// TestDashboardCSS_WizardPillHideScopedToStateCell mirrors the slop guard:
// the wizard pill replaces the plain state pill ONLY in the agent-row state
// cell (render.js), so the hide rule MUST be scoped there — an unscoped
// `body.wizard .state-pill { display: none }` would blank the Audit Outcome
// and Plugins pills too.
func TestDashboardCSS_WizardPillHideScopedToStateCell(t *testing.T) {
	if !strings.Contains(dashboardAssets, "body.wizard .state-cell .state-pill") {
		t.Error("wizard-mode pill hide is not scoped to .state-cell — would blank other tabs' pills")
	}
	// And the plain pill must NOT be hidden unscoped in wizard mode.
	if strings.Contains(dashboardAssets, "body.wizard .state-pill { display: none") {
		t.Error("wizard-mode pill hide is unscoped — will blank Audit/Plugins pills")
	}
}
