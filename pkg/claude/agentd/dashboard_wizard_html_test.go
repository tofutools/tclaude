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
