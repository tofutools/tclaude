package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// The dashboard's 2s poll used to re-render every tab by replacing a
// container's innerHTML wholesale, which destroyed the DOM under it — wiping
// any text selection every tick, so copy-paste from the dashboard was
// impossible. Legacy surfaces use dashboard/js/morph.js; migrated islands use
// Preact's keyed reconciliation. Both keep unchanged nodes untouched.
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
		if !dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — %s", needle, why)
		}
	}

	// The helper is defined and exported.
	present("export function morphInto(", "morph.js exports the morphInto reconcile helper")

	// Every remaining legacy per-tick render site reconciles via morphInto rather
	// than an innerHTML swap. Groups, Jobs and Access use keyed Preact trees.
	present("function GroupsList(", "Groups uses a Preact component instead of the custom DOM reconciler")
	present("trustedHTMLToVNodes(markup)", "Groups converts its escaped legacy renderer output into keyed VNodes")
	present("element.getAttribute('data-group-key')", "Groups promotes stable group identities to Preact keys")
	present("morphInto($('#links-list'), renderLinks(", "Links tab morphs instead of innerHTML swap")
	// The top-bar #usage widget has crossed the Preact boundary. It consumes the
	// accepted snapshot Signal and keys lines/tokens, so copyable cost/percent
	// nodes survive recurring polls without the legacy morph bridge.
	present("function Usage({ state })", "#usage is Preact-owned")
	present("usageView(state.snapshot.value?.usage)", "#usage derives from the accepted snapshot Signal")
	present("key=${line.key}", "#usage lines carry stable Preact keys")
	present("key=${token.key}", "#usage tokens carry stable Preact keys")
	for _, retired := range []string{"function renderUsage(", "morphInto(el, lines.join(''))", "morphInto(el, wins.join(''))"} {
		if dashboardSourceContains(dashboardAssets, retired) {
			t.Errorf("dashboard assets still carry retired imperative #usage painter %q", retired)
		}
	}

	// The reconcile is keyed: repeated rows carry data-key so a reorder moves
	// nodes rather than rewriting content between them. Pin the key on a couple
	// of representative row templates plus the pre-existing group key.
	present(`data-key="${esc(m.conv_id)}"`, "member rows carry a stable data-key for keyed morphing")
	present(`data-group-key=`, "group <details> keep their data-group-key match key")
	present("function JobsApp(", "Jobs uses a Preact component instead of the custom DOM reconciler")
	present("function AccessApp(", "Access uses a Preact component instead of the custom DOM reconciler")
	present("key=${grant.id}", "Access sudo rows use stable Preact keys")
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
		if !dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — %s", needle, why)
		}
	}
	gone := func(needle, why string) {
		t.Helper()
		if dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard assets still carry the retired swap %q — %s", needle, why)
		}
	}

	// Item 2 — Plugins is now a Preact island. Its component keys cards by the
	// stable plugin name; the old morph/innerHTML renderer must stay retired.
	present("key=${plugin.name} plugin=${plugin}", "installed plugin cards carry stable Preact keys")
	present("key=${plugin.name}>", "catalog plugin cards carry stable Preact keys")
	gone("morphInto($('#plugins-list'),", "legacy Plugins morph renderer remains")
	gone("$('#plugins-list').innerHTML", "Plugins list innerHTML swap regressed")

	// Item 3 — Templates management is Preact-owned. Its keyed card components
	// preserve live DOM while the unsuspended overlay refreshes behind the user;
	// the retired string renderer and morph bridge must stay gone.
	present("list.map((template) => html`<${TemplateCard} key=${template.name}", "template cards carry stable Preact keys")
	present("data-key=${template.name} data-template=${template.name}", "template cards expose their stable data key")
	gone("morphInto(host, list.map(templateCardHTML).join(''))", "legacy Templates morph renderer remains")
	gone("host.innerHTML = list.map(templateCardHTML).join('')", "Templates list innerHTML swap regressed")

	// Item 4 — Logs is Preact-owned. Reverse occurrence keys keep byte-identical
	// records unique while preserving existing rows when the live tail prepends.
	present("function LogsApp(", "Logs uses a Preact component")
	present("key=${key}", "Logs rows carry stable duplicate-safe Preact keys")
	gone("morphInto($('#logs-list'),", "legacy Logs morph renderer remains")
	gone("$('#logs-list').innerHTML", "Logs list innerHTML swap regressed")

	// Item 5 — Audit is Preact-owned and keyed by the append-only audit row id.
	present("function AuditApp(", "Audit uses a Preact component")
	present("key=${entry.id}", "Audit rows carry stable Preact keys")
	gone("morphInto($('#audit-list'),", "legacy Audit morph renderer remains")
	gone("$('#audit-list').innerHTML", "Audit list innerHTML swap regressed")

	// Item 6/7 — Costs table and summary are Preact-owned. Multi-day rows use
	// (conv_id, day) keys; the retired string/morph renderer must stay gone.
	present("key=${`${agent.conv_id}:${agent.day}`}", "cost rows carry a stable (conv_id, day) Preact key")
	gone("morphInto($('#costs-table'),", "legacy Costs table morph remains")
	gone("morphInto($('#costs-summary'),", "legacy Costs summary morph remains")

	// Item 8 — Vegas high-rollers leaderboard (slop-credits.js), keyed by conv id
	// so a rank shuffle moves the row intact.
	present(`data-key="${esc(entry.conv)}"`, "leaderboard rows carry the conv data-key (proves the morphed board)")

	// Item 9 — #meta top-bar line is Preact-owned. Its snapshot-derived component
	// keeps the URL and timestamp in distinct stable spans so selecting the URL
	// survives a poll.
	present("function FooterMeta({ state })", "#meta is Preact-owned")
	present("footerMetaView(state.snapshot.value)", "#meta derives from the accepted snapshot Signal")
	present(`id="shell-meta-root"`, "#meta has an explicit shell host")
	present(`<span class="meta-base">`, "#meta base URL lives in its own stable span")
	present(`<span class="meta-time">`, "#meta timestamp lives in its own per-tick span")
	gone("morphInto($('#meta'),", "legacy #meta morph painter remains")
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
		if !dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — animation-stamp preserve / stamp-once regressed", needle)
		}
	}
}
