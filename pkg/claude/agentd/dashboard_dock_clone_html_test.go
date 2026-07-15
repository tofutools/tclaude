package agentd

import (
	"strings"
	"testing"
)

// The palette dock's per-card ⚙ used to deep-link straight into an item's
// editor. It now opens a small actions MENU (Edit / Clone) so a preset — a
// spawn profile, a role, or a group template — can be cloned under a new name
// one click away. Like the other dashboard render guards this pins the wiring
// across HTML / CSS / JS by string-searching the embedded source rather than
// running the JS, so a rename in one file that silently breaks the dock in the
// browser fails at `go test ./...` instead. (The cross-module import graph —
// dock.js ↔ action-dialog-controller ↔ profiles/roles/templates — is verified live by
// TestDashboardModuleGraph.)
func TestDashboardHTML_DockCardCloneMenu(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}
	mustNot := func(needle, why string) {
		t.Helper()
		if strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source unexpectedly contains %q (%s)", needle, why)
		}
	}

	// --- The card ⚙ is now a menu trigger, not a direct editor deep-link. ---
	must(`data-dock-act="card-menu"`, "the card ⚙ toggles its actions menu")
	must("dock-card-menu${menuOpen ? ' open' : ''}", "the card renders its actions menu from component state")
	must(`data-dock-act="edit-item"`, "the menu's Edit item dispatches to the editor")
	must(`data-dock-act="clone-item"`, "the menu's Clone item dispatches to the clone dialog")
	must(`data-dock-act="delete-item"`, "the menu's Delete item dispatches to the delete flow")
	// Clone re-letters to "Mirror" in wizard mode, echoing the templates
	// manager's 🪞 duplicate wording; Delete → "Dispel".
	must(`wizWord('Clone', 'Mirror')`, "the Clone menu item carries the wizard vocabulary")
	must(`wizWord('Delete', 'Dispel')`, "the Delete menu item carries the wizard vocabulary")
	// The old direct deep-link act is gone — the repurpose is complete, not a
	// second parallel path.
	mustNot(`data-dock-act="manage-item"`, "the card ⚙ no longer deep-links straight to the editor")

	// The menu is wholly owned by the dock Preact component (a DISTINCT class, not
	// the shared .action-menu bus) so it can't race with row-actions.js's cog
	// machinery. Pin the component state and lifecycle that own it.
	must("const [openMenu, setOpenMenu] = useState(null);", "the dock component owns card-menu state")
	must("document.addEventListener('keydown', onKeyDown);", "the component owns Escape lifecycle")
	must("document.removeEventListener('keydown', onKeyDown);", "the component cleans up Escape lifecycle")

	// SECTIONS grew a clone hook per kind: profiles + roles use the generic
	// name dialog (openPresetCloneDialog), templates reuse their own richer duplicate
	// dialog (openDuplicateModal). A missing hook would throw on a Clone click.
	must("onCloneItem:", "the sections carry a clone hook")
	must("openPresetCloneDialog({ kind: 'profile'", "profiles clone via the generic name dialog")
	must("openPresetCloneDialog({ kind: 'role'", "roles clone via the generic name dialog")
	must("onCloneItem: (t) => openDuplicateModal(t.name)", "templates reuse their own duplicate dialog")

	// Delete reuses each kind's existing manager delete flow (confirm + delete +
	// toast). Profiles/roles get a dashboard refresh after (their removes only
	// repaint the closed manager overlay); deleteTemplate already refreshes.
	must("onDeleteItem:", "the sections carry a delete hook")
	must("removeProfile(p.name).then(() => refresh())", "profile delete reuses removeProfile + refreshes the dock")
	must("removeRole(rl.name).then(() => refresh())", "role delete reuses removeRole + refreshes the dock")
	must("onDeleteItem: (t) => deleteTemplate(t.name)", "template delete reuses deleteTemplate (which self-refreshes)")
	// The Delete item is styled destructive (red), distinct from Edit / Clone.
	must(`class="dock-card-menu-item danger"`, "the Delete menu item is marked destructive")
	must(".dock-card-menu-item.danger {", "the destructive Delete item has a red skin")
	must("body.wizard #agent-dock .dock-card-menu-item.danger", "the Delete item's destructive skin re-skins in wizard mode")

	// Keyed Preact card and Groups menus survive snapshot publishes, so neither
	// participates in the narrow transient-editor refresh guard.
	mustNot(`if (document.querySelector('.action-menu.open')) return true;`, "Preact-owned action menus must not suspend refresh")
	mustNot(".action-menu.open, .dock-card-menu.open", "Preact-owned dock menus do not suspend refresh")

	// --- The Preact-owned generic clone dialog (#clone-modal). ----------------
	must(`id="clone-modal"`, "the clone dialog exists")
	must(`id="clone-modal-name"`, "the clone dialog has a new-name field")
	must(`id="clone-modal-submit"`, "the clone dialog has a submit button")
	// Title / blurb are JS-driven (one dialog serves both profile + role kinds).
	must(`id="clone-modal-title"`, "the clone dialog's JS-driven title target exists")
	must(`id="clone-modal-blurb"`, "the clone dialog's JS-driven blurb target exists")
	must("export function openPresetCloneDialog(options)", "the compatibility controller exports the opener")
	must("descriptor.kind === 'preset-clone'", "the action-dialog island renders the clone shell")
	must("return state.openPresetClone(options)", "plain actions publish clone opens into state")
	mustNot("modal-clone.js", "the retired imperative clone module has no consumers")
	// The clone POSTs the source object with the name swapped, then refreshes
	// so the dock's snapshot-driven cards show the new one at once.
	must("const payload = { ...source, name };", "the clone re-POSTs the source object with the name swapped")
	must("refresh()", "a successful clone refreshes the dock")

	// --- Wizard styling (the operator flagged this twice). ------------------
	// The menu chrome re-skins in wizard mode, kept SCOPED under #agent-dock (the
	// anti-pin invariant — no unscoped body.wizard widening from this feature).
	must("body.wizard #agent-dock .dock-card-menu {", "the card menu has a wizard skin scoped under #agent-dock")
	must("body.wizard #agent-dock .dock-card-menu-item", "the menu items have a wizard skin")
	// Menu items get an explicit dark skin so a bare <button> doesn't render
	// browser-white (the documented dashboard gotcha).
	must(".dock-card-menu-item {", "the menu items carry an explicit (non-white) dark skin")
	// The dialog itself gets a per-#id wizard skin like every other modal — an
	// unstyled one would fall back to plain dark + a white submit button.
	must("body.wizard #clone-modal .cron-create-modal {", "the clone dialog has a per-#id wizard chrome")
	must("body.wizard #clone-modal #clone-modal-submit {", "the clone dialog's submit is gilded in wizard mode")
	// Keep the ⚙ visible while its menu is open even after the pointer leaves.
	must(".dock-card:has(.dock-card-menu.open) .dock-card-manage", "the ⚙ stays visible while its menu is open")
}
