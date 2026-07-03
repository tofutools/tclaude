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
