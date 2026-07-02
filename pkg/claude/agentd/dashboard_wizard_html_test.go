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

	// The marquee shows a short random sample per scroll pass, not the whole
	// quote pool joined into one wall of text: the pool + the per-pass sampler
	// exist, and the re-roll happens at the scroll-loop boundary
	// (animationiteration) so quotes never shuffle mid-scroll.
	must("const TICKER_QUOTES = [", "wizard-fx.js carries the ticker quote pool")
	must("const TICKER_QUOTES_PER_PASS", "the marquee samples a fixed few quotes per pass")
	must("function rollTickerQuotes()", "the per-pass sampler exists")
	must("rollTickerQuotes();\n    updateMarqueeText(text);", "the sample re-rolls at the scroll-loop boundary, then repaints")
	must("rollTickerQuotes();\n      updateMarqueeText(text);", "a theme flip into wizard mode re-rolls + repaints too")

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
	// (.ga-wizard) with fantasy glyphs by default (or opt-in pixel sprites),
	// emitted alongside regular + slop and CSS-swapped in via body.wizard
	// (same "always emit, theme picks" trick).
	must("export function wizardBotsHTML(", "group-activity.js exports the wizard glyph row")
	must("styledWizardBotsHTML,", "render.js imports the wizard bot-row switchboard")
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

// TestDashboardHTML_WizardTabNames pins the arcane nav-tab names — every tab
// keeps its plain name normally and shows a wizard "chamber of the Tower" name
// under body.wizard (⚔ Parties, ⏳ Rituals, 📜 Almanac, …), the sibling of the
// Vegas→Tavern swap. Both spans always emit; a pure-CSS swap picks the active
// one. Purely front-end like the rest of the wizard theme, so we string-search
// the embedded source rather than running the JS.
func TestDashboardHTML_WizardTabNames(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The CSS swap: the default-hide of the arcane span (shared with Vegas) is
	// load-bearing — without it BOTH labels would render side by side outside
	// wizard mode. The new rule hides the PLAIN label under body.wizard; the
	// reveal rule shows the arcane one. Pin all three so a dropped line fails CI
	// rather than the browser.
	must(".tab-label-wizard { display: none; }", "the arcane tab label is hidden outside wizard mode")
	must("body.wizard .tab-label-regular { display: none; }", "wizard mode hides the plain tab names")
	must("body.wizard .tab-label-wizard { display: inline; }", "wizard mode shows the arcane tab names")

	// Each tab's plain + arcane span pair. Pinning the full pair (through the
	// data-tab so it's anchored to the right button) guards both the plain name
	// staying honest and the arcane name being present.
	pairs := []struct{ tab, regular, wizard string }{
		{"groups", "Groups", "⚔ Parties"},
		{"terminals", "Terminals", "🔮 Scrying"},
		{"jobs", "Jobs", "⚒ Labours"},
		{"plugins", "Plugins", "🔧 Contraptions"},
		{"access", "Access", "🛡 Wards"},
		{"messages", "Messages", "🕊 Missives"},
		{"costs", "Costs", "💰 Coffers"},
		{"audit", "Audit", "📖 Chronicle"},
		{"logs", "Logs", "🕯 Runes"},
		{"config", "Config", "📜 Almanac"},
	}
	for _, p := range pairs {
		must(`<span class="tab-label-regular">`+p.regular+`</span>`, p.tab+" keeps its plain nav label")
		must(`<span class="tab-label-wizard">`+p.wizard+`</span>`, p.tab+" gets its arcane nav label")
	}

	// The command palette's "Go to …/Scry the …" command reads the VISIBLE
	// label span (not btn.textContent, which would concatenate both spans and
	// the badge count) so it names the tab per the active theme.
	must("const wizEl = btn.querySelector('.tab-label-wizard');", "the palette finds the arcane tab label span")
	must("const regEl = btn.querySelector('.tab-label-regular, .tab-label-vegas');", "the palette finds the plain tab label span")
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

	// Mid-submit, the disabled state re-skins the ::before to "🔮 Summoning…"
	// (rather than surfacing the JS "Spawning…" fallback) so the in-progress
	// copy stays in voice with the "🔮 Summon!" button it started as.
	must(`content: "🔮 Summoning…"`, "the in-progress button reads Summoning in wizard mode")

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

	// Busy state: the JS spinner fallback injects "Retiring…" text, but a
	// familiar is *banished*, not retired — the disabled button paints a
	// themed "Banishing…" via ::after (pure-CSS, like the idle ::before label).
	must(`content: "Banishing…"`, "the busy confirm button reads Banishing in wizard mode")

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

// TestDashboardHTML_WizardEnterBanner pins the "It's wizard time!" enter banner
// wiring — the flash shown when the operator flips INTO wizard mode from another
// theme. Unlike the per-spawn summon banner it carries one fixed line (no
// rotation), reusing the same banner + shower style. Purely front-end like the
// rest of the wizard theme, so we string-search the embedded source rather than
// running the JS — a dropped call or a renamed export would otherwise lose the
// feature silently in the browser.
func TestDashboardHTML_WizardEnterBanner(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Public surface + both sides of the wiring: wizard-fx exports the enter
	// banner + its edge-event binder, and dashboard.js installs that binder at
	// bootstrap.
	must("export function wizardEnter(", "wizard-fx.js exports the enter-mode banner")
	must("export function bindWizardEnterBanner(", "wizard-fx.js exports the enter-banner binder")
	must("bindWizardEnterBanner();", "dashboard.js installs the enter banner at bootstrap")

	// The banner rides slop.js's tclaude:wizard edge event, firing ONLY on the
	// into-wizard edge (detail.active === true). Pin the gate so a refactor that
	// drops the active check — flashing the banner when LEAVING wizard mode too —
	// trips here.
	must("if (e.detail && e.detail.active) wizardEnter();", "the enter banner fires only on the into-wizard edge")

	// The enter line is a fixed constant, not a pick from the rotating summon
	// pool — pin the constant so a refactor that wires the enter banner to
	// SUMMON_QUOTES (reintroducing rotation) fails here.
	must("const ENTER_WIZARD_QUOTE = '🧙 It", "the enter banner carries the fixed It's-wizard-time line")

	// It reuses the summon banner's font-clamp variant so the two share a look.
	must(".wizard-summon-banner", "the enter banner reuses the summon font-clamp variant")
}

// TestDashboardHTML_WizardPowerButtons pins the wizard re-skin of the Power On
// / Shutdown chips at both scopes: global (top-bar #power-on-all-btn /
// #shutdown-all-btn) and per-group (.group-actions data-act="power-on-group" /
// "shutdown-group"). In 🧙 mode they become gilded "✨ Awaken" / "🌙 Slumber"
// levers (green/red tinted so the go/stop meaning survives) with a per-theme
// visible-label swap. Purely front-end like the rest of the wizard theme, so we
// string-search the embedded source rather than running the JS.
func TestDashboardHTML_WizardPowerButtons(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Global (top-bar) chips: re-skinned by id so body.wizard out-specifies both
	// the base #…-btn rule and the flat `body.wizard header button` skin.
	must("body.wizard #power-on-all-btn", "the global Power On chip is re-skinned in wizard mode")
	must("body.wizard #shutdown-all-btn", "the global Shutdown chip is re-skinned in wizard mode")

	// Per-group chips: re-skinned via the data-act attribute so the extra
	// classes/attr out-specify the base .group-actions button (and .warn) rules.
	must(`body.wizard .group-actions button[data-act="power-on-group"]`, "the per-group Power On chip is re-skinned in wizard mode")
	must(`body.wizard .group-actions button[data-act="shutdown-group"]`, "the per-group Shutdown chip is re-skinned in wizard mode")

	// Per-theme visible-label swap (same span-pair trick as the Summon button):
	// both spans emitted, CSS picks which shows.
	must(".pwr-label-wizard { display: none; }", "the wizard power label is hidden by default")
	must("body.wizard .pwr-label-regular { display: none; }", "wizard mode hides the plain power labels")
	must("body.wizard .pwr-label-wizard { display: inline; }", "wizard mode shows the arcane power labels")

	// The wizard-flavoured copy at both scopes — Awaken (power on) / Slumber
	// (shutdown). Global carries the "…all" variant, per-group the bare verb.
	must(`<span class="pwr-label-wizard">✨ Awaken all</span>`, "the global Power On chip reads Awaken in wizard mode")
	must(`<span class="pwr-label-wizard">🌙 Slumber all</span>`, "the global Shutdown chip reads Slumber in wizard mode")
	must(`<span class="pwr-label-wizard">✨ Awaken</span>`, "the per-group Power On chip reads Awaken in wizard mode")
	must(`<span class="pwr-label-wizard">🌙 Slumber</span>`, "the per-group Shutdown chip reads Slumber in wizard mode")

	// The regular-theme labels stay honest (both spans always emitted).
	must(`<span class="pwr-label-regular">🟢 power on all</span>`, "the global Power On chip keeps its plain label")
	must(`<span class="pwr-label-regular">🛑 shutdown</span>`, "the per-group Shutdown chip keeps its plain label")

	// WCAG 2.5.3 Label-in-Name: since the visible label swaps per theme, the
	// aria-label must contain BOTH the wizard word AND the *exact* regular
	// visible phrase (so a speech-input user saying either matches). In
	// particular the one-word "shutdown" — not "shut down" — has to appear, or
	// the regular-theme fallback fails the guarantee.
	must(`aria-label="Awaken all — power on all offline agents on the dashboard"`, "global Power On aria-label carries both 'Awaken all' and 'power on all'")
	must(`aria-label="Slumber all — shutdown all running agents on the dashboard"`, "global Shutdown aria-label carries both 'Slumber all' and 'shutdown all'")
	must(`aria-label="Awaken — power on every offline agent in this group"`, "per-group Power On aria-label carries both 'Awaken' and 'power on'")
	must(`aria-label="Slumber — shutdown every running agent in this group"`, "per-group Shutdown aria-label carries both 'Slumber' and 'shutdown'")
}

// TestDashboardHTML_WizardProfileEditor pins the wizard re-skin of the
// spawn-profile editor ("Inscribe a summoning recipe"): a spawn profile is a
// saved recipe the Summon dialog pre-fills from, so its editor is themed as
// inscribing that recipe — the same arcane chrome as the Summon dialog. Purely
// front-end like the rest of the wizard theme, so we string-search the embedded
// source rather than running the JS.
func TestDashboardHTML_WizardProfileEditor(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The whole dialog is re-skinned arcane, scoped to the profile editor modal.
	must("body.wizard #profile-editor-modal .cron-create-modal", "the profile editor surface is re-skinned")

	// The submit button becomes a "📜 Inscribe!" lever (Summon's twin) — the
	// ::before glyph + the modal-scoped submit re-skin.
	must(`content: "📜 Inscribe!"`, "the submit button reads Inscribe in wizard mode")
	must("body.wizard #profile-editor-modal #profile-editor-submit", "the submit re-skin is scoped to the profile editor modal")
}

// TestDashboardCSS_WizardProfileEditorScoped guards that the wizard
// profile-editor re-skin stays scoped to #profile-editor-modal — the editor is
// a .cron-create-modal like the spawn dialog and its siblings, so an unscoped
// `body.wizard .cron-create-modal { … }` would repaint every one of them. This
// duplicates TestDashboardCSS_WizardSpawnModalScoped's assertion, but keeping a
// dedicated guard here documents that BOTH .cron-create-modal re-skins depend
// on the same scoping invariant.
func TestDashboardCSS_WizardProfileEditorScoped(t *testing.T) {
	if strings.Contains(dashboardAssets, "body.wizard .cron-create-modal {") {
		t.Error("wizard profile-editor re-skin is unscoped — will repaint spawn/clone/reincarnate/cron modals too")
	}
}

// TestDashboardHTML_WizardProfilesManage pins the wizard re-skin of the "Spawn
// profiles" management overlay — the picker you choose a profile to edit from,
// the sibling of the arcane editor it launches. Purely front-end like the rest
// of the wizard theme, so we string-search the embedded source.
func TestDashboardHTML_WizardProfilesManage(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The overlay surface + the gilded "+ new profile" action, both scoped.
	must("body.wizard #profiles-manage-modal .manage-modal", "the profiles overlay surface is re-skinned")
	must("body.wizard #profiles-manage-modal #profile-create-open.primary", "the + new profile button gets the gilded-arcane treatment")
	must("body.wizard #profiles-manage-modal .template-card", "the profile cards are re-skinned")
}

// TestDashboardCSS_WizardProfilesManageScoped guards that the wizard
// profiles-overlay re-skin stays scoped to #profiles-manage-modal — it's a
// .manage-modal shared with the Templates… / Links… overlays, so an unscoped
// `body.wizard .manage-modal { … }` would repaint those too.
func TestDashboardCSS_WizardProfilesManageScoped(t *testing.T) {
	if strings.Contains(dashboardAssets, "body.wizard .manage-modal {") {
		t.Error("wizard profiles-overlay re-skin is unscoped — will repaint the Templates/Links overlays too")
	}
}

// TestDashboardHTML_WizardProfileVocabulary pins the wizard copy swap that
// re-letters a "spawn profile" as a "familiar pattern" — because in 🧙 wizard
// mode we Summon Familiars, so the profiles dialog and every spot that names it
// should say so. The static spots use a shared .profiles-word-regular /
// .profiles-word-wizard span pair (CSS-swapped, like the spawn/retire titles);
// the JS-rendered spots (the editor title, the empty-state, the default-profile
// picker's "+ new" option) swap via the shared wizWord() helper. Covers both the
// profiles dialog itself and the three profile selectors (spawn dialog / global
// default / group default). All of it lands in the embedded source, so we
// string-search rather than run the JS.
func TestDashboardHTML_WizardProfileVocabulary(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The shared CSS reveal for the .profiles-word span pair.
	must(".profiles-word-wizard { display: none; }", "the wizard profile-word span is hidden by default")
	must("body.wizard .profiles-word-regular { display: none; }", "wizard hides the default profile wording")
	must("body.wizard .profiles-word-wizard { display: inline; }", "wizard shows the familiar-pattern wording")

	// The static span-swap spots: the manage overlay title, its "+ new" action,
	// the Groups-cog ⧉ entry, and the Summon dialog's "Save as…" button.
	must(`<span class="profiles-word-wizard">Familiar patterns</span>`, "the manage overlay title reads 'Familiar patterns' in wizard mode")
	must(`<span class="profiles-word-wizard">+ new pattern</span>`, "the + new action reads '+ new pattern' in wizard mode")
	must(`<span class="profiles-word-wizard">⧉ patterns…</span>`, "the Groups-cog entry reads '⧉ patterns…' in wizard mode")
	must(`<span class="profiles-word-wizard">Save as pattern…</span>`, "the Save-as button reads 'Save as pattern…' in wizard mode")
	// The regular twin still ships (so non-wizard mode is unchanged).
	must(`<span class="profiles-word-regular">Spawn profiles</span>`, "the default manage overlay title still reads 'Spawn profiles'")

	// The JS-rendered spots swapped via wizWord() (editor title + empty-state).
	must("New familiar pattern", "the editor title reads 'New familiar pattern' when creating in wizard mode")
	must("Edit pattern: ${seed.name}", "the editor title reads 'Edit pattern: <name>' when editing in wizard mode")
	must("No familiar patterns yet", "the empty-state reads 'No familiar patterns yet' in wizard mode")

	// The three profile *selectors* also speak the vocabulary in wizard mode:
	//   - spawn dialog: the "Profile" row label (static .profiles-word span pair)
	//   - global + group default: the "＋ new profile…" option in the shared
	//     openProfilePicker <select> (JS, wizWord).
	must(`<span class="profiles-word-wizard">Pattern</span>`, "the spawn dialog's Profile selector label reads 'Pattern' in wizard mode")
	must("＋ new pattern…", "the global/group default picker's new-entry reads '＋ new pattern…' in wizard mode")
}

// TestDashboardHTML_WizardPermEditor pins the wizard re-skin of the permanent-
// permissions tri-state editor (#perm-edit-modal), shared by the live-agent path
// and the spawn / profile buffer editors. Front-end only, so we string-search
// the embedded source.
func TestDashboardHTML_WizardPermEditor(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The editor surface is re-skinned arcane, scoped to the perm-edit modal.
	must("body.wizard #perm-edit-modal .perm-edit-modal", "the permissions editor surface is re-skinned")

	// The active Grant/Deny tri-state fills are the safety signal and must stay
	// their base green/red. The re-skin protects them by repainting only the
	// INACTIVE cells (:not(.active)) — pin that mechanism so a refactor to a
	// blanket `.perm-tristate button` rule (which would swallow the green/red via
	// the id's specificity) trips here.
	must("body.wizard #perm-edit-modal .perm-tristate button:not(.active)", "only inactive tri-state cells are repainted (Grant/Deny keep their green/red)")
}

// TestDashboardCSS_WizardPermEditorScoped guards that the wizard perm-editor
// re-skin never leaks onto the generic tri-state control: .perm-tristate /
// .perm-row are plain classes (the perm editor is the only user today, but they
// are not id-bound), so every wizard rule must carry the #perm-edit-modal id. An
// unscoped `body.wizard .perm-tristate button { … }` would repaint any tri-state
// anywhere — and, worse, out-specify the base active Grant/Deny fills and clobber
// the green/red safety signal.
func TestDashboardCSS_WizardPermEditorScoped(t *testing.T) {
	if strings.Contains(dashboardAssets, "body.wizard .perm-tristate button {") {
		t.Error("wizard perm-editor tri-state re-skin is unscoped — would leak onto any tri-state control and clobber Grant/Deny fills")
	}
}

// TestDashboardHTML_WizardGrimoireCopy pins the naming layer on top of the
// perm-editor chrome re-skin: the dialog and its controls are re-lettered to
// match the "Grimoire…" button that opens them. #694 themed the CHROME (see
// TestDashboardHTML_WizardPermEditor); this covers only the COPY — "Edit
// permanent permissions" → "📕 The Grimoire", "all default" → "unbind all",
// "Cancel" → "Dispel", and the Save primary → "✒ Bind!". Pure-CSS span swaps
// plus a ::before glyph, so we string-search the embedded source.
func TestDashboardHTML_WizardGrimoireCopy(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Title copy: a pure-CSS span swap, "Edit permanent permissions" → "📕 The Grimoire".
	must(`<span class="perm-edit-title-regular">Edit permanent permissions</span>`, "the default perm-editor title span")
	must(`<span class="perm-edit-title-wizard">📕 The Grimoire</span>`, "the wizard perm-editor title span")
	must("body.wizard #perm-edit-title .perm-edit-title-regular", "wizard hides the default title")
	must("body.wizard #perm-edit-title .perm-edit-title-wizard", "wizard shows the Grimoire title")

	// Secondary buttons swap their labels: all default → unbind all, Cancel → Dispel.
	must(`<span class="pe-btn-wizard">unbind all</span>`, "the reset button reads unbind all in wizard mode")
	must(`<span class="pe-btn-wizard">Dispel</span>`, "the Cancel button reads Dispel in wizard mode")
	must("body.wizard #perm-edit-modal .pe-btn-regular", "wizard hides the default button labels")
	must("body.wizard #perm-edit-modal .pe-btn-wizard", "wizard shows the arcane button labels")

	// The Save primary is re-lettered to a "✒ Bind!" glyph (on #694's lever),
	// with a "✒ Binding…" in-progress glyph on the disabled (mid-save) state.
	must(`content: "✒ Bind!"`, "the Save button reads Bind in wizard mode")
	must(`content: "✒ Binding…"`, "the mid-save button reads Binding in wizard mode")
	must("body.wizard #perm-edit-modal #perm-edit-submit { font-size: 0; }", "the Save text is hidden so the glyph shows")
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

// TestDashboardHTML_WizardConfirmModal pins the wizard re-skin of the shared
// confirm dialog (#confirm-modal — confirmModal in refresh.js), the element the
// move-agent-to-another-group / remove-from-group / backdrop-discard confirms
// pop. Its title / body / OK label are JS-set per action, so unlike the retire
// dialog there is no copy swap — only the chrome is re-skinned; string-search
// the embedded CSS for the modal-scoped rules.
func TestDashboardHTML_WizardConfirmModal(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Needles are " {"-anchored so a prefix match on a sibling rule (e.g. the
	// h3 / :hover variants) can't satisfy them — same trick as the
	// unscoped-needle guards above.
	must("body.wizard #confirm-modal .modal {", "the confirm dialog surface is re-skinned")
	must("body.wizard #confirm-modal #confirm-ok {", "the OK button gets the gilded lever, scoped to the confirm modal")
	must("body.wizard #confirm-modal .modal-buttons button:not(#confirm-ok) {", "Cancel gets the tarnished-gold secondary treatment")
}

// TestDashboardHTML_WizardEditMemberModal pins the wizard re-skin of the
// edit-agent dialog ("Enchant this familiar") — the dialog the per-agent "+ role"
// cell and the ⚙ "edit" affordance open. Like the spawn / retire re-skins it is a
// pure-CSS span swap (title + the two secondary button labels) plus a ::before
// glyph on the Save primary, so we string-search the embedded source.
func TestDashboardHTML_WizardEditMemberModal(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Title copy: a pure-CSS span swap, "Edit agent" → "Enchant this familiar".
	must(`<span class="edit-member-title-regular">Edit agent</span>`, "the default edit-agent title span")
	must(`<span class="edit-member-title-wizard">Enchant this familiar</span>`, "the wizard edit-agent title span")
	must("body.wizard #edit-member-title .edit-member-title-regular", "wizard hides the default title")
	must("body.wizard #edit-member-title .edit-member-title-wizard", "wizard shows the enchant title")

	// Secondary buttons swap their labels the same way: Permissions… → Grimoire…,
	// Cancel → Dispel.
	must(`<span class="em-btn-wizard">Grimoire…</span>`, "the Permissions button reads Grimoire in wizard mode")
	must(`<span class="em-btn-wizard">Dispel</span>`, "the Cancel button reads Dispel in wizard mode")
	must("body.wizard #edit-member-modal .em-btn-regular", "wizard hides the default button labels")
	must("body.wizard #edit-member-modal .em-btn-wizard", "wizard shows the arcane button labels")

	// The Save primary becomes a "✨ Enchant!" gilded lever (Summon / Banish's twin)
	// — the ::before glyph + the modal-scoped re-skin.
	must(`content: "✨ Enchant!"`, "the Save button reads Enchant in wizard mode")
	must("body.wizard #edit-member-modal #edit-member-save", "the Save re-skin is scoped to the edit-member modal")

	// The whole dialog surface is re-skinned arcane.
	must("body.wizard #edit-member-modal .modal", "the edit-agent dialog surface is re-skinned")
}

// TestDashboardCSS_WizardEditMemberModalScoped guards that the wizard edit-agent
// re-skin stays scoped to #edit-member-modal. The edit-agent modal is a generic
// .modal (like the retire modal), so an unscoped `body.wizard .modal { … }` would
// repaint every sibling confirm dialog. The shared retire-scope test already
// rejects that literal; this asserts the positive — the re-skin is present and
// carries the #edit-member-modal scope prefix.
func TestDashboardCSS_WizardEditMemberModalScoped(t *testing.T) {
	if !strings.Contains(dashboardAssets, "body.wizard #edit-member-modal .modal") {
		t.Error("wizard edit-agent re-skin missing its #edit-member-modal scope prefix")
	}
	if strings.Contains(dashboardAssets, "body.wizard .modal {") {
		t.Error("wizard edit-agent re-skin is unscoped — will repaint sibling confirm dialogs too")
	}
}

// TestDashboardHTML_WizardSummonButton pins the wizard re-skin of the
// group-header spawn button (the blue .spawn-btn that opens the dialog): the
// per-theme label swap "spawn" → "🔮 Summon" and the arcane chrome hooks.
// Purely front-end like the rest of the wizard theme, so we string-search the
// embedded source rather than running the JS.
func TestDashboardHTML_WizardSummonButton(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Label copy: a pure-CSS span swap emitted by render.js, mirroring the
	// dialog title's .spawn-title-regular/.spawn-title-wizard pair. Both spans
	// must exist in the rendered button and the swap rules in CSS.
	must(`<span class="spawn-btn-label-regular">spawn</span>`, "the default spawn-button label span")
	must(`<span class="spawn-btn-label-wizard">🔮 Summon</span>`, "the wizard spawn-button label span")
	must("body.wizard .spawn-btn .spawn-btn-label-regular", "wizard hides the default button label")
	must("body.wizard .spawn-btn .spawn-btn-label-wizard", "wizard shows the Summon button label")

	// The SVG glyph gives way to the 🔮 in the wizard label.
	must("body.wizard .spawn-btn .spawn-ico", "wizard hides the button's SVG glyph")

	// The button chrome is re-skinned arcane (gilded gradient + parchment text).
	must("body.wizard .spawn-btn {", "the spawn button gets the arcane chrome re-skin")
}

// TestDashboardHTML_WizardPartyButton pins the wizard re-skin of the filter
// bar's "+ new group" primary ("⚔ Form a party"): the per-theme label span
// swap, the button re-skin, and the matching swap in the "No groups yet…"
// empty-state hint. Front-end only, so we string-search the embedded source.
func TestDashboardHTML_WizardPartyButton(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Label copy: a pure-CSS span swap, "+ new group" → "⚔ Form a party".
	// Both spans must exist and the swap rules must be present in CSS.
	must(`<span class="group-create-label-regular">+ new group</span>`, "the default new-group label span")
	must(`<span class="group-create-label-wizard">⚔ Form a party</span>`, "the wizard party label span")
	// The default-hide rule is load-bearing: without it the wizard label has no
	// rule in the default/slop theme and BOTH labels would render side by side.
	// Pin the literal so a dropped line fails CI rather than the browser.
	must(".group-create-label-wizard { display: none; }", "the default theme hides the wizard label")
	must("body.wizard .group-create-label-regular", "wizard hides the default label")
	must("body.wizard .group-create-label-wizard", "wizard shows the party label")

	// The button itself is re-skinned arcane, scoped to its id so the base
	// .filter-bar button.primary rule still styles it in the default theme.
	must("body.wizard #group-create-open.primary", "the party button is re-skinned in wizard mode")

	// The same span pair swaps the empty-state hint that names the button, so
	// "No groups yet…" reads "⚔ Form a party" in wizard mode too. render.js
	// emits both variants; the swap rules above reveal the active one. Pin the
	// full markup (through the wizard span) so dropping the wizard variant from
	// the hint alone still fails — a looser prefix needle would pass on the
	// button's identical regular span.
	must(`No groups yet. Create one with the <strong><span class="group-create-label-regular">+ new group</span><span class="group-create-label-wizard">⚔ Form a party</span></strong>`, "the empty-state hint carries both swappable labels")
}

// TestDashboardHTML_WizardGroupCreateDialog pins the wizard re-skin of the
// dialog the "⚔ Form a party" button opens. The button was themed (covered by
// TestDashboardHTML_WizardPartyButton above), but the modal it opens kept the
// default chrome — this pins that the dialog surface, title span-swap and
// submit-lever glyph are all re-skinned arcane. Front-end only, so we
// string-search the embedded source (html + css) rather than running the JS.
func TestDashboardHTML_WizardGroupCreateDialog(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Title: a pure-CSS span swap, "Create a new agent group" → "⚔ Form a
	// party" (echoing the button, like the cron title echoes its opener). Both
	// spans + the swap rules must be present.
	must(`<span class="group-create-title-regular">Create a new agent group</span>`, "the default group-create title span")
	must(`<span class="group-create-title-wizard">⚔ Form a party</span>`, "the wizard group-create title span")
	// The default-hide rule is load-bearing: without it the wizard title has no
	// rule in the default/slop theme and BOTH titles render side by side.
	must(".group-create-title-wizard { display: none; }", "the default theme hides the wizard title")
	must("body.wizard #group-create-title .group-create-title-regular", "wizard hides the default title")
	must("body.wizard #group-create-title .group-create-title-wizard", "wizard shows the party title")

	// The dialog surface is re-skinned, scoped to #group-create-modal so the
	// sibling .cron-create-modal dialogs keep the default chrome.
	must("body.wizard #group-create-modal .cron-create-modal", "the group-create dialog surface is re-skinned")
	// The submit's static "Create" text is hidden and swapped for a ::before
	// glyph lever, with a themed in-flight (disabled) copy.
	must("body.wizard #group-create-modal #group-create-submit {", "the submit button becomes a gilded lever")
	must(`content: "⚔ Form the party!";`, "the submit-lever copy")
	must(`content: "⚔ Gathering the party…";`, "the in-flight submit copy")
	// The number (Max members) input is re-skinned alongside the text/textarea
	// fields so no field stays a bright default-dark against the violet.
	must("body.wizard #group-create-modal .cron-create-row input[type=number]", "the number field is re-skinned")
	// Non-primary buttons (Cancel / Browse…) get the secondary arcane skin.
	must("body.wizard #group-create-modal button:not(.primary)", "non-primary buttons get the secondary arcane skin")
}

// TestDashboardCSS_WizardGroupCreateModalScoped guards that the wizard
// group-create dialog re-skin stays scoped to #group-create-modal — the dialog
// shares the .cron-create-modal class with the spawn / clone / reincarnate /
// cron / message-create modals, so an unscoped rule would repaint all of them.
// Mirrors TestDashboardCSS_WizardSpawnModalScoped / …WizardCronDialogScoped; the
// shared unscoped-needle guard below covers all three, but a dedicated check
// documents this dialog's dependence on the scoping too.
func TestDashboardCSS_WizardGroupCreateModalScoped(t *testing.T) {
	if strings.Contains(dashboardAssets, "body.wizard .cron-create-modal {") {
		t.Error("wizard group-create re-skin is unscoped — will repaint spawn/clone/cron modals too")
	}
}

// TestDashboardHTML_WizardCronDialog pins the wizard re-skin of the cron tab's
// "+ new cron job" flow ("⏳ Bind a recurring ritual"): the per-theme open-button
// label span swap + its empty-state hint twin, the arcane dialog surface, the
// JS-set title/submit glyph copy (create *and* edit via the .cron-editing mode
// class), and the picked-chip highlight. Front-end only, so we string-search the
// embedded source (html + css + js) rather than running the JS.
func TestDashboardHTML_WizardCronDialog(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Open button: a pure-CSS span swap, "+ new cron job" → "⏳ Bind a
	// recurring ritual". Both spans and the swap rules must be present.
	must(`<span class="cron-open-label-regular">+ new cron job</span>`, "the default open-button label span")
	must(`<span class="cron-open-label-wizard">⏳ Bind a recurring ritual</span>`, "the wizard open-button label span")
	// The default-hide rule is load-bearing: without it the wizard label has no
	// rule in the default/slop theme and BOTH labels render side by side.
	must(".cron-open-label-wizard { display: none; }", "the default theme hides the wizard label")
	must("body.wizard .cron-open-label-regular", "wizard hides the default label")
	must("body.wizard .cron-open-label-wizard", "wizard shows the ritual label")
	must("body.wizard #cron-create-open.primary", "the open button is re-skinned in wizard mode")
	must("body.wizard #cron-create-open.primary:hover", "the open button has a wizard hover re-skin")

	// The same span pair swaps the Jobs tab's "No jobs yet…" empty-state hint
	// that names the button (tabs.js emits both variants). Pin the full markup
	// through the wizard span so dropping the wizard variant still fails.
	must(`schedule a cron job with the <strong><span class="cron-open-label-regular">+ new cron job</span><span class="cron-open-label-wizard">⏳ Bind a recurring ritual</span></strong>`, "the empty-state hint carries both swappable labels")

	// The dialog surface + title are re-skinned, scoped to #cron-create-modal so
	// the sibling .cron-create-modal dialogs keep the default chrome.
	must("body.wizard #cron-create-modal .cron-create-modal", "the cron dialog surface is re-skinned")
	// Title/submit copy is JS-set, so it's swapped via font-size:0 + ::before
	// glyphs, with the create/edit split driven by the .cron-editing mode class.
	must(`body.wizard #cron-create-modal #cron-create-title::before {`, "the title uses a ::before glyph swap")
	must(`content: "⏳ Bind a recurring ritual";`, "create-mode title copy")
	must(`content: "⏳ Re-bind the ritual";`, "edit-mode title copy")
	must("body.wizard #cron-create-modal.cron-editing #cron-create-title::before", "edit-mode title is keyed on the .cron-editing class")
	must(`content: "⏳ Bind it!";`, "create-mode submit-lever copy")
	must(`content: "⏳ Re-bind it!";`, "edit-mode submit-lever copy")
	must("body.wizard #cron-create-modal #cron-create-submit {", "the submit button becomes a gilded lever")
	// The brief in-flight (disabled) submit copy stays in the arcane voice — pin
	// both mode variants so a dropped busy label fails CI rather than the browser.
	must(`content: "⏳ Binding…";`, "create-mode in-flight submit copy")
	must(`content: "⏳ Re-binding…";`, "edit-mode in-flight submit copy")

	// The JS toggles the mode class on the modal (a MODE flag, not a theme read).
	must("$('#cron-create-modal').classList.add('cron-editing')", "edit-open JS sets the edit mode class")
	must("$('#cron-create-modal').classList.remove('cron-editing')", "create-open JS clears the edit mode class")

	// The picked "Every" chip keeps a distinct gilded highlight over the row of
	// tarnished-gold chips. Excluding the submit via :not(.primary) (not
	// :not(#id)) is what lets this .selected rule out-specify the chip skin.
	must("body.wizard #cron-create-modal .cron-schedule-chips button.selected", "the picked schedule chip gets a gilded highlight")
	must("body.wizard #cron-create-modal button:not(.primary)", "non-primary buttons get the secondary arcane skin")
}

// TestDashboardCSS_WizardCronDialogScoped guards that the wizard cron-dialog
// re-skin stays scoped to #cron-create-modal — the dialog shares the
// .cron-create-modal class with the spawn / clone / reincarnate / group-import
// / message-create modals, so an unscoped rule would repaint all of them. This
// mirrors TestDashboardCSS_WizardSpawnModalScoped; the shared unscoped-needle
// guard below covers both, but a dedicated check documents this dialog's
// dependence on the scoping too.
func TestDashboardCSS_WizardCronDialogScoped(t *testing.T) {
	if strings.Contains(dashboardAssets, "body.wizard .cron-create-modal {") {
		t.Error("wizard cron re-skin is unscoped — will repaint spawn/clone/message modals too")
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

// TestDashboardHTML_WizardCommandPalette pins the wizard re-skin of the
// Ctrl/⌘-K command palette ("📖 The Spellbook"): the wizard-only title bar,
// the arcane CSS surface, the launcher-glyph swap, and the JS-driven
// placeholder / empty-state copy. Purely front-end like the rest of the
// wizard theme, so we string-search the embedded source rather than running
// the JS.
func TestDashboardHTML_WizardCommandPalette(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The title bar: present in the HTML, hidden by default, revealed +
	// styled only under body.wizard. "The Spellbook" is the palette's
	// wizard-mode name, the sibling of "The Wizard's Tower" page title.
	must(`<div class="palette-title" aria-hidden="true">📖 The Spellbook</div>`,
		"the wizard-mode Spellbook title bar exists in the palette markup")
	must(".palette-title { display: none; }", "the title is hidden outside wizard mode")
	must("body.wizard #command-palette-modal .palette-title",
		"wizard mode reveals + styles the Spellbook title")

	// The whole palette is re-skinned arcane, scoped to the palette overlay.
	must("body.wizard #command-palette-modal .palette-box",
		"the palette surface is re-skinned in wizard mode")

	// The header 🔍 launcher becomes a 📖 spellbook via the ::before glyph
	// swap (the same technique the Summon / Banish levers use).
	must("body.wizard #command-palette-btn", "the launcher button is re-skinned in wizard mode")
	must(`content: "📖";`, "the launcher shows a spellbook glyph in wizard mode")

	// The placeholder + empty-state copy are wizard-flavoured by palette.js
	// (attribute/text can't be swapped in CSS), gated on isWizardActive().
	must("const WIZARD_PLACEHOLDER", "palette.js defines the wizard placeholder copy")
	must("const WIZARD_EMPTY", "palette.js defines the wizard empty-state copy")
	must("input.placeholder = isWizardActive() ? WIZARD_PLACEHOLDER : defaultPlaceholder",
		"the palette swaps the placeholder per theme on open")
	must("isWizardActive() ? WIZARD_EMPTY", "the no-match line is wizard-flavoured")
	// The theme copy is centralised so a theme flip WHILE the palette is open
	// (the +W hotkey fires with focus in the box) re-applies it — the CSS
	// chrome swaps instantly, so the JS copy must follow rather than lag a
	// theme behind until the next open.
	must("function applyThemeCopy(", "the palette centralises the theme-flavoured copy")
}

// TestDashboardHTML_WizardCommandPaletteSynonyms pins the wizard re-flavour of
// the palette's COMMANDS themselves (not just the chrome): every presented
// label + hint re-skins arcane under body.wizard via the wiz() helper, while
// the plain verbs stay in the keywords and the scorer's SYNONYMS map bridges
// the arcane words to the plain ones (and back) so old search terms never stop
// working. Purely front-end like the rest of the wizard theme, so we
// string-search the embedded source rather than running the JS.
func TestDashboardHTML_WizardCommandPaletteSynonyms(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The helper that picks the arcane vs plain PRESENTED string per theme,
	// read live so the tclaude:wizard listener's rebuild re-skins an open list.
	must("function wiz(regular, wizard) {", "palette.js defines the per-theme presented-copy helper")
	must("return isWizardActive() ? wizard : regular;", "wiz() returns the arcane string only in wizard mode")

	// A representative arcane label from each command family — the presented
	// wording that re-skins the Spellbook. (The plain twin in each pair is
	// pinned by TestDashboardHTML_CommandPalette.)
	must("'Veil all familiars'", "hide-windows presents as Veil in wizard mode")
	must("'Reveal all familiars'", "focus-windows presents as Reveal in wizard mode")
	must("'Slumber all familiars'", "global shutdown presents as Slumber in wizard mode")
	must("'Awaken all familiars'", "global power-on presents as Awaken in wizard mode")
	must("'Summon a familiar…'", "spawn presents as Summon in wizard mode")
	must("'Dispel banished familiars…'", "delete-retired presents as Dispel in wizard mode")
	must("'Banish unbound familiars…'", "retire-ungrouped presents as Banish unbound familiars in wizard mode")
	must("`Banish familiar: ${label}`", "per-agent retire presents as Banish in wizard mode")
	must("`Scry the ${name}`", "tab navigation presents as Scry in wizard mode")
	must("`Furl coven: ${g.name}`", "group collapse presents as Furl in wizard mode")
	must("`Prune stray branches in ${g.name}`", "worktree cleanup presents as Prune in wizard mode")

	// The arcane vocabulary rides in the keywords (always, both themes) so a
	// wizard-minded search finds a plain-labelled command too — spot-check a
	// few of the noun/verb sets appended to the plain keyword strings.
	must("summon conjure invoke call forth familiar", "spawn carries the arcane summon keywords")
	must("slumber sleep rest lull dormant quell still familiars", "global shutdown carries the arcane slumber keywords")
	must("banish exile dismiss familiar", "per-agent retire carries the arcane banish keywords")
	must("veil conceal cloak shroud portal scrying vision familiars", "hide-windows carries the arcane veil keywords")

	// A mid-open theme flip must re-skin the baked labels, not just the
	// placeholder — the tclaude:wizard listener rebuilds the command list AND
	// resets the selection before re-rendering. Pin the exact 3-line sequence
	// (rebuild → reset → re-render with the live query): openPalette also calls
	// `commands = buildCommands();` but follows it with `input.value = ''`, not
	// `render(input.value)`, so this needle is unique to the listener — deleting
	// the listener's rebuild fails here rather than passing on openPalette's copy.
	must("commands = buildCommands();\n    selected = 0;\n    render(input.value);",
		"the tclaude:wizard listener rebuilds + resets selection so labels re-skin live without a stale highlight")

	// The scorer's SYNONYMS map bridges the arcane verbs to the plain ones so
	// typing either vocabulary finds the command in either theme.
	must("summon: ['spawn']", "summon bridges to spawn")
	must("slumber: ['shutdown', 'stop']", "slumber bridges to shutdown/stop")
	must("awaken: ['resume']", "awaken bridges to resume")
	must("banish: ['retire']", "banish bridges to retire")
	must("veil: ['hide']", "veil bridges to hide")
	must("reveal: ['focus', 'show']", "reveal bridges to focus/show")
}

// TestDashboardCSS_WizardCommandPaletteScoped guards that the wizard palette
// re-skin stays scoped to #command-palette-modal — an unscoped
// `body.wizard .palette-box { … }` is the kind of leak the sibling
// spawn/retire guards catch, so pin the intent here too even though
// .palette-box is currently palette-unique.
func TestDashboardCSS_WizardCommandPaletteScoped(t *testing.T) {
	if strings.Contains(dashboardAssets, "body.wizard .palette-box {") {
		t.Error("wizard palette re-skin is unscoped — should be scoped to #command-palette-modal")
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

// TestDashboardHTML_WizardCogs pins the wizard re-skin of the ⚙ "more actions"
// cogs — the filter-bar "more group actions" cog, the per-row cog and the
// per-group-header cog all become gilded, self-turning "enchanted clockwork"
// gears in 🧙 mode. The glyph rides a .cog-glyph span so only the gear spins,
// not the bordered box. Purely front-end like the rest of the wizard theme, so
// we string-search the embedded source rather than running the JS.
func TestDashboardHTML_WizardCogs(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Both emit sites wrap the gear in a .cog-glyph span: actionCog (row/group
	// menus, helpers.js) and the filter-bar button (dashboard.html). Without the
	// span the CSS would have to rotate the whole bordered button box.
	must(`<span class="cog-glyph">⚙︎</span>`, "the cog glyph rides a .cog-glyph span so only the gear rotates")

	// All three cogs get the gilded arcane skin.
	must("body.wizard .filter-bar-cog .cog-btn", "the filter-bar cog is re-skinned in wizard mode")
	must("body.wizard .row-actions .cog-btn", "the per-row cog is re-skinned in wizard mode")
	must("body.wizard .group-actions .cog-btn", "the per-group cog is re-skinned in wizard mode")

	// The enchanted rotation + its keyframes — the "self-turning clockwork" gag.
	must("animation: wizard-cog-turn", "the wizard cog glyph spins under its own enchantment")
	must("@keyframes wizard-cog-turn", "the cog-turn keyframes are defined")

	// The rotation is dropped under prefers-reduced-motion (belt-and-suspenders
	// CSS layer), scoped to the .cog-glyph so the gilded skin/glow survives.
	if !strings.Contains(dashboardAssets, "body.wizard .group-actions .cog-btn .cog-glyph { animation: none; }") {
		t.Error("wizard cog rotation is not disabled under prefers-reduced-motion")
	}
}

// TestDashboardHTML_WizardConfigTab pins the wizard re-skin of the Config tab
// ("📜 The Wizard's Almanac"): the wizard-only title, the arcane re-skin of the
// sections / headings / inputs / sticky footer + Save lever, and the matching
// diff-confirm dialog. Purely front-end like the rest of the wizard theme, so
// we string-search the embedded source rather than running the JS.
func TestDashboardHTML_WizardConfigTab(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The wizard-only title: present in the HTML, hidden by default, revealed +
	// styled only under body.wizard — the sibling of the Spellbook / Grimoire
	// titles. Pin the default-hide rule too (load-bearing: without it the title
	// would show in every theme).
	must(`<h2 class="cfg-wizard-title" aria-hidden="true">📜 The Wizard's Almanac</h2>`,
		"the wizard-mode Almanac title exists in the config markup")
	must(".cfg-wizard-title { display: none; }", "the title is hidden outside wizard mode")
	must("body.wizard .cfg-wizard-title", "wizard mode reveals + styles the Almanac title")

	// The tab chrome is re-skinned arcane, scoped to #tab-config so no other tab
	// is touched.
	must("body.wizard #tab-config .cfg-section", "the config sections are re-skinned in wizard mode")
	must("body.wizard #tab-config .cfg-section > h3", "the section headings are re-skinned")
	must("body.wizard #tab-config .cfg-field input[type=text]", "the config inputs are re-skinned")
	must("body.wizard #tab-config .cfg-footer", "the sticky footer is re-skinned")
	must("body.wizard #tab-config .cfg-footer button.primary", "the Save lever gets the gilded-arcane treatment")

	// The diff-confirm dialog is re-skinned too, scoped to #config-diff-modal.
	must("body.wizard #config-diff-modal .config-diff-modal", "the diff-confirm surface is re-skinned")
	must("body.wizard #config-diff-modal .modal-buttons button.primary", "the diff-confirm Save lever is re-skinned")
}

// TestDashboardCSS_WizardConfigTabScoped guards that the config-tab re-skin
// stays scoped. The re-skin recolours .filter-bar / .primary chrome that other
// tabs also use, so an unscoped `body.wizard .filter-bar input { … }` or
// `body.wizard .cfg-footer button.primary { … }`-shaped leak would repaint the
// Groups / Costs filter bars (or beyond). Every config rule must carry the
// #tab-config (or #config-diff-modal) scope.
func TestDashboardCSS_WizardConfigTabScoped(t *testing.T) {
	// The live filter input is the highest-leak selector — .filter-bar is shared
	// by every tab. An unscoped wizard rule for it would bleed everywhere.
	if strings.Contains(dashboardAssets, "body.wizard .filter-bar input {") {
		t.Error("wizard config filter re-skin is unscoped — will repaint every tab's filter bar")
	}
	// The confirm button's re-skin must not leak to sibling .primary buttons.
	if strings.Contains(dashboardAssets, "body.wizard .cfg-footer button.primary {") {
		t.Error("wizard config footer must be scoped to #tab-config, not bare .cfg-footer")
	}
}
