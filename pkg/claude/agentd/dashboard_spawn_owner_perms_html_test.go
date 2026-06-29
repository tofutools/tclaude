package agentd

import (
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
	present(`id="profile-editor-owner"`, "profile editor has the owner tri-state")
	present(`id="profile-editor-perms"`, "profile editor has the Permissions… button")

	// The shared editor gained a buffer (no-conv) mode + its pre-spawn opener.
	present("function openPermEditor(", "the permission editor has a shared renderer")
	present("function openSpawnPermEditor(", "the buffer (pre-spawn) editor opener exists")
	present("openSpawnPermEditor", "the spawn/profile dialogs invoke the buffer editor")

	// The spawn body carries the birth-time access controls.
	present("body.is_owner = true", "the spawn body sends is_owner when checked")
	present("body.permission_overrides = spawnPermOverrides",
		"the spawn body sends the buffered overrides")

	// The profile payload carries them too (tri-state owner + overrides).
	present("body.is_owner = owner", "the profile payload sends the tri-state owner")
	present("body.permission_overrides = profilePermOverrides",
		"the profile payload sends its buffered overrides")
}
