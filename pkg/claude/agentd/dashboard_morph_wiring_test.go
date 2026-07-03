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

	// Every per-tick render site the poll hits reconciles via morphInto rather
	// than an innerHTML swap. tabs.js drives the four list tabs; refresh.js the
	// two Access sub-panels.
	present("morphInto($('#groups-list'), renderGroups(", "Groups tab morphs instead of innerHTML swap")
	present("morphInto($('#jobs-list'), renderJobs(", "Jobs tab morphs instead of innerHTML swap")
	present("morphInto($('#sudo-list'), renderSudo(", "Sudo tab morphs instead of innerHTML swap")
	present("morphInto($('#links-list'), renderLinks(", "Links tab morphs instead of innerHTML swap")
	present("morphInto($('#permissions-body'), renderPermissions(", "Permissions panel morphs instead of innerHTML swap")
	present("morphInto($('#slugs-body'), renderSlugs(", "Slugs panel morphs instead of innerHTML swap")

	// The reconcile is keyed: repeated rows carry data-key so a reorder moves
	// nodes rather than rewriting content between them. Pin the key on a couple
	// of representative row templates plus the pre-existing group key.
	present(`data-key="${esc(m.conv_id)}"`, "member rows carry a stable data-key for keyed morphing")
	present(`data-key="sudo-${esc(String(r.id))}"`, "sudo rows carry a stable data-key for keyed morphing")
	present(`data-group-key=`, "group <details> keep their data-group-key match key")

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
