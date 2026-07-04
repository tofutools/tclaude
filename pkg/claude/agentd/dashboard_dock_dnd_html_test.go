package agentd

import (
	"strings"
	"testing"
)

// JOH-375 2/4 (js/dock-dnd.js): dragging a palette-dock PROFILE or ROLE card
// onto a group opens the spawn dialog prefilled (target group + profile/role).
// Like the other dashboard render guards this pins the wiring across HTML / CSS
// / JS by string-searching the embedded source rather than running the JS, so a
// rename in one file that silently breaks the drag in the browser fails at
// `go test ./...` instead. It is a pure-frontend feature — no server endpoint,
// no schema — so this string pin is the coverage.
func TestDashboardHTML_DockDnd(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The module + its two exports (mirrors dnd.js / group-reorder.js).
	must("export { bindDockDnd, dockDragActive };", "dock-dnd.js exports its binder + active flag")
	must("bindDockDnd();", "dashboard.js boot wires the dock drag")
	must("import { bindDockDnd } from './dock-dnd.js';", "dashboard.js imports the dock drag binder")

	// The drag source: profile + role sections are draggable; the card sets the
	// draggable attribute off the section's `drag` flag.
	must("drag: true,", "a dock section opts into being a drag source")
	must(`draggable="${draggable}"`, "cards render draggable off the section flag")

	// Custom MIME (NOT text/plain) — the isolation contract with dnd.js: a dock
	// drop carries this and only this, and dnd.js's member-drop bails on it.
	must("application/x-tclaude-dock-item", "the dock drag uses its own custom MIME")
	must("e.dataTransfer.types.includes('application/x-tclaude-dock-item')",
		"dnd.js's member-drop bails on a dock drop (MIME isolation)")

	// The 2s morph guard: dockDragActive suspends auto-refresh for the whole
	// gesture (refresh.js reads it, mirroring dndDragActive / groupReorderActive).
	must("import { dockDragActive } from './dock-dnd.js';", "refresh.js imports the dock drag flag")
	must("if (dockDragActive) return true;", "auto-refresh suspends while a dock drag is in flight")

	// The drop opens the EXISTING spawn dialog prefilled — no new endpoint, no
	// clone of the modal. Group-drop pins the group; the profile/role rides in
	// via openAgentSpawnModal's opts (threaded through initSpawnProfileSelector).
	must("openAgentSpawnModal(opts)", "the drop opens the existing spawn modal with prefill opts")
	must("profileName: (opts && opts.profileName) || '',", "the spawn modal accepts a preselected profile")
	must("if (forceRole) $('#agent-spawn-role').value = forceRole;", "the spawn modal presets a dropped role")

	// Drop-target highlight in BOTH skins, wizard rule SCOPED to this feature's
	// selectors (the anti-pin invariant — no unscoped body.wizard widening).
	must("#groups-list.dock-drop-over {", "the empty-space drop target has a highlight rule")
	must("details[data-dnd-target-group].dock-drop-over,", "the group-box drop target has a highlight rule")
	must(".dock-card.dock-drag-source { opacity: 0.5; }", "the dragged card fades while in flight")
	must("body.wizard details[data-dnd-target-group].dock-drop-over,",
		"the wizard drop-highlight is scoped under this feature's selectors")
}
