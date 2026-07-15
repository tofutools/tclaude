package agentd

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// This change put a "Group owner" checkbox + a "Permissions…" button on the spawn
// dialog's Role row, and the same controls on the spawn-profile editor. Both
// open the SAME per-slug permission editor the live-agent path uses, in a
// buffered (pre-spawn) mode, and fold the result into the spawn / profile body.
// That wiring is entirely in the embedded dashboard JS/HTML, so no server path
// proves it — this guards the shape. spawn_owner_perms_flow_test.go is the
// companion that exercises the endpoint behaviour.
func TestDashboardSpawnOwnerPermsUI_Wired(t *testing.T) {
	present := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// Spawn dialog — the owner checkbox + Permissions… button on the Role row.
	present(`id="agent-spawn-owner"`, "spawn dialog has the Group-owner checkbox")
	present(`id="agent-spawn-perms"`, "spawn dialog has the Permissions… button")
	present(`id="agent-spawn-perms-indicator"`, "spawn dialog has the overrides indicator")

	// Profile editor — the tri-state owner + Permissions… button.
	present(`'profile-editor-owner'`, "profile editor has the owner tri-state")
	present(`id="profile-editor-perms"`, "profile editor has the Permissions… button")

	// The shared editor gained a buffer (no-conv) mode + its pre-spawn opener.
	present("function PermissionsDialog(", "the permission editor has one shared Preact renderer")
	present("export function openSpawnPermEditor(options = {})", "the controller exposes the buffer editor opener")
	present("openBufferedPermissions(options = {})", "the state owns a keyed buffered permission launch")
	present("openSpawnPermEditor", "the spawn/profile dialogs invoke the buffer editor")

	// The spawn body carries the birth-time access controls.
	present("body.is_owner = true", "the spawn body sends is_owner when checked")
	present("body.permission_overrides = { ...draft.permissionOverrides }",
		"the spawn body sends the buffered overrides")

	// The profile payload carries them too (tri-state owner + overrides).
	present("['include_group_default_context', draft.include_group_default_context], ['is_owner', draft.is_owner]", "the profile payload includes the tri-state owner")
	present("body.permission_overrides = { ...draft.permission_overrides }",
		"the profile payload sends its buffered overrides")
}

// TestDashboardPermEditorStacksAboveProfileEditor guards an Escape-correctness
// invariant. The buffered permission editor (#perm-edit-modal) now opens from
// the spawn-profile editor's Permissions… button — i.e. stacked ON TOP of
// #profile-editor-modal. Escape dismissal routes through isTopmostOverlay, which
// ranks overlays by computed z-index and breaks ties by DOM order. The perm
// editor comes EARLIER in the DOM than the profile editor, so if the two share a
// z-index the tiebreak names the profile editor topmost and Escape would close
// the editor BENEATH the visible one. The fix is a strictly higher z-index on
// the perm editor; this test fails if a future edit ever re-equalizes them.
func TestDashboardPermEditorStacksAboveProfileEditor(t *testing.T) {
	zIndexOf := func(selector string) int {
		t.Helper()
		// Match `#selector { … z-index: N; … }` (single-line rule).
		re := regexp.MustCompile(regexp.QuoteMeta(selector) + `\s*\{[^}]*z-index:\s*(\d+)`)
		m := re.FindStringSubmatch(dashboardAssets)
		if m == nil {
			t.Fatalf("no z-index rule found for %s", selector)
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("bad z-index for %s: %v", selector, err)
		}
		return n
	}

	perm := zIndexOf("#perm-edit-modal")
	profile := zIndexOf("#profile-editor-modal")
	if perm <= profile {
		t.Errorf("#perm-edit-modal z-index (%d) must be strictly above #profile-editor-modal (%d) "+
			"so Escape closes the perm editor stacked on top of it, not the editor beneath", perm, profile)
	}
}
