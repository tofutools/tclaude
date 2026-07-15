package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_RemoteControlDefaultsWired guards the two JOH-262
// "remote-control defaults" dashboard controls:
//
//  1. The spawn-profile editor's tri-state `remote_control` toggle — a *bool
//     (unset / off / on) that rides into the profile create/edit body as
//     `remote_control`, gated on the chosen harness's Remote Access capability
//     (the inverse of the Codex-only trust-dir toggle it sits beside).
//  2. The group remote-control-policy control — a STRING enum
//     (inherit / optin / deny) that PATCHes the group's `remote_control_policy`
//     through the same /api/groups/{name} endpoint the default_profile /
//     notify_enabled chips use. It lives as a click-to-cycle item in the group
//     ⚙ menu (not a header chip).
//
// The pieces span dashboard.html (the profile row), modal-profiles.js + the
// profiles.js summary (the profile toggle), groups-list.js (the group cog menu item)
// and row-actions.js (the item's PATCH dispatch). A rename in any of them silently
// breaks the control in the browser, and the repo has no JS test runner, so
// this asserts on the embedded asset concatenation at `go test ./...` — the
// same guard style as TestDashboardHTML_SpawnRemoteControlWired.
func TestDashboardHTML_RemoteControlDefaultsWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// ---- 1. Spawn-profile editor: tri-state remote_control ---------------

	// dashboard.html: the editor row + its tri-state <select>.
	must(`id="profile-editor-remote-control"`, "profile editor has a remote-control select")

	// modal-profiles.js: the row gates on the harness capability (shown for a
	// Remote-Access harness like Claude, hidden for one without it like Codex),
	// the same can_remote_control flag the spawn dialog reads.
	must("hidden=${hEntry && !hEntry.can_remote_control}", "profile remote-control row gates on the harness capability")

	// modal-profiles.js: it reads the tri-state in (round-trips unset/off/on)
	// and the body carries `remote_control` only when set AND the harness
	// supports it (gated on the harness's can_remote_control).
	must("remote_control: triValue(seed?.remote_control)",
		"the editor reads the profile's remote_control tri-state on open")
	must("readTri(draft.remote_control)", "the payload reads the remote-control tri-state")
	must("body.remote_control = remote", "the profile body carries remote_control when set")
	must("(!h || h.can_remote_control)",
		"the body opt-in is gated on the harness's Remote Access capability")

	// profiles.js: the profile card summary surfaces an explicit remote_control.
	must("`remote-control ${p.remote_control ? 'on' : 'off'}`",
		"the profile card summary shows the remote-control state when set")

	// ---- 2. Group remote-control-policy cog-menu item --------------------

	// groups-list.js: the native menu component reads the wire token off the
	// group, and cycles inherit → optin → deny (the three wire tokens the group
	// PATCH accepts).
	must("function GroupMenuItems({ group, members, snapshot })", "the group menu component is defined")
	must("group.remote_control_policy || 'inherit'", "the item reads the group's remote_control_policy")
	must("policy === 'inherit' ? 'optin' : policy === 'optin' ? 'deny' : 'inherit'",
		"the item cycles inherit → optin → deny")
	must(`data-act="set-group-remote-control"`, "the item dispatches the group remote-control action")
	// It renders as a button INTO the group ⚙ menu (groupMenuItems), not as
	// a header chip — the move that decluttered the group summary.
	must("data-policy=${policy} data-next=${nextPolicy}", "the policy item is wired into the group cog menu")

	// row-actions.js: the handler PATCHes the group endpoint with the
	// remote_control_policy field (same endpoint + method as default_profile).
	must("case 'set-group-remote-control':", "the item has a dispatch case")
	must("`/api/groups/${encodeURIComponent(group)}`", "the item PATCHes the group endpoint")
	must("JSON.stringify({ remote_control_policy: next })", "the item sends the remote_control_policy body")
}
