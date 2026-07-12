package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// The dashboard's 2s poll used to re-render every tab by replacing a
// container's innerHTML wholesale, which destroyed the DOM under it — wiping
// any text selection every tick, so copy-paste from the dashboard was
// impossible. The fix (dashboard/js/morph.js) reconciles the freshly-rendered
// HTML against the live DOM (keyed DOM morphing) so unchanged nodes are never
// touched and a selection survives the poll.
//
// The morph is pure client-side JS with no server path of its own, so — like
// the other dashboard wiring guards in this package — this pins the pieces by
// string-searching the embedded assets. A rename in one file that isn't
// mirrored in its call sites would otherwise only surface in the browser.
//
// The reconciler's BEHAVIOUR (keyed reorder, in-place text update, identity
// preservation, <details open> preservation) is covered by the Node unit
// suite jstest/morph.test.mjs, run by TestPaletteScore_JS under `go test`.
func TestDashboardMorph_Wired(t *testing.T) {
	// The module ships embedded (it lives under js/, so //go:embed dashboard
	// captures it and the module-glob serves it) — without this the imports
	// below would 404 at runtime.
	if _, err := fs.ReadFile(dashboardAssetsFS, "js/morph.js"); err != nil {
		t.Fatalf("embedded js/morph.js missing: %v", err)
	}

	present := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — %s", needle, why)
		}
	}

	// The helper is defined and exported.
	present("export function morphInto(", "morph.js exports the morphInto reconcile helper")

	// Every remaining legacy per-tick render site reconciles via morphInto rather
	// than an innerHTML swap. Jobs is intentionally absent: its Preact subtree
	// has its own keyed reconciliation and component coverage.
	present("morphInto($('#groups-list'), renderGroups(", "Groups tab morphs instead of innerHTML swap")
	present("morphInto($('#sudo-list'), renderSudo(", "Sudo tab morphs instead of innerHTML swap")
	present("morphInto($('#links-list'), renderLinks(", "Links tab morphs instead of innerHTML swap")
	present("morphInto($('#permissions-body'), renderPermissions(", "Permissions panel morphs instead of innerHTML swap")
	present("morphInto($('#slugs-body'), renderSlugs(", "Slugs panel morphs instead of innerHTML swap")
	// The top-bar #usage widget (render.js) — first item of the coverage sweep
	// (JOH-339): both the Codex two-line and the Claude single-row branches morph
	// so the copyable cost/percent figures survive the tick. The `usage: n/a`
	// textContent branch stays a plain write (nothing to preserve).
	present("morphInto(el, lines.join(''))", "#usage Codex layout morphs instead of innerHTML swap")
	present("morphInto(el, wins.join(''))", "#usage Claude layout morphs instead of innerHTML swap")

	// The reconcile is keyed: repeated rows carry data-key so a reorder moves
	// nodes rather than rewriting content between them. Pin the key on a couple
	// of representative row templates plus the pre-existing group key.
	present(`data-key="${esc(m.conv_id)}"`, "member rows carry a stable data-key for keyed morphing")
	present(`data-key="sudo-${esc(String(r.id))}"`, "sudo rows carry a stable data-key for keyed morphing")
	present(`data-group-key=`, "group <details> keep their data-group-key match key")
	present("function JobsApp(", "Jobs uses a Preact component instead of the custom DOM reconciler")
	present("key=${`cron-${row.cron?.id}`}", "Jobs cron rows use stable Preact keys")
	present("key=${`export-${row.export?.id}`}", "Jobs export rows use stable Preact keys")

	// The old wholesale swaps at these six sites must be gone — their return
	// would silently re-break copy-paste while every other guard stayed green.
	for _, gone := range []string{
		"$('#groups-list').innerHTML = renderGroups(",
		"$('#jobs-list').innerHTML = renderJobs(",
		"$('#sudo-list').innerHTML = renderSudo(",
		"$('#links-list').innerHTML = renderLinks(",
		"$('#permissions-body').innerHTML = renderPermissions(",
		"$('#slugs-body').innerHTML = renderSlugs(",
	} {
		if strings.Contains(dashboardAssets, gone) {
			t.Errorf("dashboard assets still carry the retired innerHTML swap %q — copy-paste fix regressed", gone)
		}
	}
}

// TestDashboardMorph_MailWired guards the extension of the morph to the Messages
// tab — the ORIGINAL copy-paste complaint (a message body in the reader pane
// deselecting every 2s). paintMail's per-tick panes now reconcile via morphInto
// instead of an innerHTML swap, message-list rows are keyed, and the reconciler
// carries the form-control property special case the bulk-select checkboxes need.
func TestDashboardMorph_MailWired(t *testing.T) {
	present := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — %s", needle, why)
		}
	}

	// morph.js gained the form-control property sync + the fast-path bypass that
	// makes it correct for the bulk-select checkboxes (see syncFormProps).
	present("function syncFormProps(", "morph.js syncs live form-control properties from the fresh render")
	present("if (!isFormControl && from.isEqualNode(to)) return;", "form controls bypass the isEqualNode fast path")

	// mail.js's per-tick paint panes morph instead of swapping innerHTML.
	present("morphInto(el, html);", "paintSidebar morphs the sidebar")
	present("morphInto(el, filtered.map(m => {", "paintList morphs the message list")
	present("morphInto(el, `\n    <div class=\"mail-reader-head\">", "paintReader morphs the reader (primary copy surface)")
	present("morphInto(bar, `${nav}<span class=\"grow\"></span>${sizeSel}`)", "paintPager morphs the pager")

	// Message-list rows carry a stable data-key (the unique message id) so a new
	// message arriving moves rows intact instead of morphing neighbour rows.
	present(`data-key="${m.id}" data-kind=`, "message rows carry a data-key for keyed morphing")

	// The old wholesale message-list swap is gone — its return would silently
	// re-break the very use case this extension targets.
	if strings.Contains(dashboardAssets, "el.innerHTML = filtered.map(m => {") {
		t.Error("mail.js still carries the retired `el.innerHTML = filtered.map(...)` list swap — Messages copy-paste regressed")
	}
}

// TestDashboardMorph_SweepWired guards the coverage sweep (JOH-339 items 2–9):
// the remaining recurring innerHTML sites that a re-poll (2s snapshot, or the
// slower logs-stream / audit-repoll / costs-repoll ticks) rebuilt wholesale,
// each now reconciled via morphInto so a selection survives its tick. Like the
// guards above these pin the wiring by string-searching the embedded assets, so
// a de-wiring (a morphInto reverted to an innerHTML swap, or a data-key dropped)
// surfaces in `go test` instead of only in the browser.
func TestDashboardMorph_SweepWired(t *testing.T) {
	present := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — %s", needle, why)
		}
	}
	gone := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets still carry the retired swap %q — %s", needle, why)
		}
	}

	// Item 2 — Plugins is now a Preact island. Its component keys cards by the
	// stable plugin name; the old morph/innerHTML renderer must stay retired.
	present("key=${plugin.name} plugin=${plugin}", "installed plugin cards carry stable Preact keys")
	present("key=${plugin.name}>", "catalog plugin cards carry stable Preact keys")
	gone("morphInto($('#plugins-list'),", "legacy Plugins morph renderer remains")
	gone("$('#plugins-list').innerHTML", "Plugins list innerHTML swap regressed")

	// Item 3 — Templates management overlay list (modal-templates.js). The
	// overlay is a .manage-overlay, deliberately not refresh-suspended, so it
	// repaints while read; keyed by template name.
	present("morphInto(host, list.map(templateCardHTML).join(''))", "Templates list morphs instead of innerHTML swap")
	present(`data-key="${esc(t.name)}" data-template=`, "template cards carry a stable data-key")
	gone("host.innerHTML = list.map(templateCardHTML).join('')", "Templates list innerHTML swap regressed")

	// Item 4 — Logs tab table (logs.js). No stable per-line id (a burst can emit
	// byte-identical lines), so rows are matched POSITIONALLY — a paged table,
	// acceptable per the ticket. All three #logs-list writes route through morph.
	present("morphInto($('#logs-list'),", "Logs list morphs instead of innerHTML swap")
	gone("$('#logs-list').innerHTML", "Logs list innerHTML swap regressed")

	// Item 5 — Audit tab table (audit.js), keyed by the append-only audit row id
	// (unique per entry) so a fresh command at the top moves survivors intact.
	present("morphInto($('#audit-list'),", "Audit list morphs instead of innerHTML swap")
	present(`data-key="audit-${esc(String(e.id))}"`, "audit rows carry the row-id data-key")
	gone("$('#audit-list').innerHTML", "Audit list innerHTML swap regressed")

	// Item 6/7 — Costs table and summary are Preact-owned. Multi-day rows use
	// (conv_id, day) keys; the retired string/morph renderer must stay gone.
	present("key=${`${agent.conv_id}:${agent.day}`}", "cost rows carry a stable (conv_id, day) Preact key")
	gone("morphInto($('#costs-table'),", "legacy Costs table morph remains")
	gone("morphInto($('#costs-summary'),", "legacy Costs summary morph remains")

	// Item 8 — Vegas high-rollers leaderboard (slop-credits.js), keyed by conv id
	// so a rank shuffle moves the row intact.
	present(`data-key="${esc(conv)}"`, "leaderboard rows carry the conv data-key (proves the morphed board)")

	// Item 9 — #meta top-bar line (refresh.js): split into a stable URL span and
	// a per-tick timestamp span so selecting the URL survives the poll.
	present("morphInto($('#meta'),", "#meta morphs instead of a wholesale textContent write")
	present(`<span class="meta-base">`, "#meta base URL lives in its own stable span")
	present(`<span class="meta-time">`, "#meta timestamp lives in its own per-tick span")
	gone("$('#meta').textContent = data.popup_base", "#meta wholesale textContent write regressed")
}

// TestDashboardMorph_AnimationStampsPreserved guards the fix for the eval
// finding that activity-bot + wizard-orbit animations reset every tick under
// morph. Under wholesale innerHTML the re-phasers (syncBotAnimations /
// syncWizardOrbit) stamped a wall-clock animation-delay on every freshly-created
// node each tick; under morph the nodes PERSIST, so (a) re-stamping a running
// node shifts its phase (a visible jump) and (b) morphAttributes would strip the
// stamp the fresh render never emits. The fix: morphAttributes PRESERVES the
// post-pass-owned inline style (animation-delay + --wizard-orbit-delay), and the
// re-phasers became stamp-once (skip an already-stamped node). Both halves must
// stay in lockstep — a drop in either reopens the per-tick reset.
func TestDashboardMorph_AnimationStampsPreserved(t *testing.T) {
	for _, needle := range []string{
		// morph.js — morphAttributes re-applies the live stamps the fresh side lacks.
		"if (liveDelay && !to.style.animationDelay) st.animationDelay = liveDelay;",
		"st.setProperty('--wizard-orbit-delay', liveOrbit);",
		// helpers.js — the re-phasers re-stamp only when the animation IDENTITY
		// changed (a (re)start), keyed on a per-node signature, so a stable node
		// keeps its stamp but a status change (different keyframes/period) re-locks.
		"const sig = cs.animationName + ' ' + period;",
		"if (botStampSig.get(el) === sig && el.style.animationDelay) continue;",
		// helpers.js — the wizard orbit clears its stamp when the pill leaves a
		// channeling status, so a later return re-phases from that restart.
		"orbitStampSig.delete(pill);",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — animation-stamp preserve / stamp-once regressed", needle)
		}
	}
}
