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
	mustNot := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source still contains %q (%s)", needle, why)
		}
	}

	// The module exports its disposable binder. Its active flag remains private:
	// keyed Preact dock nodes survive refreshes, so the shared poll no longer
	// needs to observe or suspend around the gesture.
	must("export { bindDockDnd };", "dock-dnd.js exports its binder")
	must("pageCleanups.push(bindDockDnd());", "dashboard.js registers the disposable dock drag binder with the shared page lifecycle")
	must("import { bindDockDnd } from './dock-dnd.js';", "dashboard.js imports the dock drag binder")

	// The drag source: profile + role sections are draggable; the card sets the
	// draggable attribute off the section's `drag` flag.
	must("drag: true,", "a dock section opts into being a drag source")
	must("draggable=${draggable}", "cards render draggable off the section flag")

	// Custom MIME (NOT text/plain) — the isolation contract with dnd.js: a dock
	// drop carries this and only this, and dnd.js's member-drop bails on it.
	must("application/x-tclaude-dock-item", "the dock drag uses its own custom MIME")
	must("e.dataTransfer.types.includes('application/x-tclaude-dock-item')",
		"dnd.js's member-drop bails on a dock drop (MIME isolation)")

	// Preact owns keyed dock cards, so refresh continues during the gesture.
	// The private flag still routes document-level drag events and cleanup.
	must("let dockDragActive = false;", "dock-dnd.js keeps private gesture routing state")
	mustNot("import { dockDragActive } from './dock-dnd.js';", "refresh.js must not import private dock drag state")
	mustNot("if (dockDragActive) return true;", "dock drags must not suspend auto-refresh")

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

	// The virtual UNGROUPED box is a no-group drop target (a discoverable synonym
	// for the empty-space drop): dockTarget accepts it as { group: '' }, and it
	// gets the drop highlight in both skins. Every OTHER virtual box stays inert.
	must("e.target.closest('details[data-dnd-target-ungrouped]')",
		"dockTarget accepts the virtual Ungrouped box as a no-group drop target")
	must("details[data-dnd-target-ungrouped].dock-drop-over,",
		"the Ungrouped drop target has a highlight rule")
	must("body.wizard details[data-dnd-target-ungrouped].dock-drop-over,",
		"the wizard Ungrouped drop-highlight is scoped under this feature's selectors")
}
