package agentd

import (
	"strings"
	"testing"
)

// The dashboard's Groups tab collapses each agent row's and each group
// header's less-used buttons behind a ⚙ "options" cog: clicking the cog
// opens a small .action-menu of the collected actions. This is entirely
// client-side JS (groups-interactions.js / groups-list.js /
// groups-member-table.js)
// with no daemon behaviour change, so — like the other dashboard render
// guards — this test pins the wiring by string-searching the embedded
// dashboard source rather than running the JS.
func TestDashboardHTML_OptionsMenu(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The native cog + collapsed-menu wiring. ActionMenu receives the concrete
	// row/group kind at its keyed call sites.
	must(`class="cog-btn"`, "the ⚙ cog renders as a .cog-btn button")
	must(`kind="row-menu"`, "agent rows carry a ⚙ options cog (row-menu)")
	must(`kind="group-menu"`, "group headers carry a ⚙ options cog (group-menu)")
	must("function ActionMenu(", "one native component owns every Groups action menu")
	must(".action-menu {", "the .action-menu dropdown has a CSS rule")
	must(".action-menu.open", "an open menu is shown via the .open class")

	// The per-agent control cluster: the status dot + focus/hide + cog
	// share one cell (.agent-ctl); focus/hide are eye icons, disabled
	// (not hidden) on an offline agent.
	must(`class="agent-ctl"`, "the status dot + action cluster share one cell")
	must(".agent-ctl {", "the combined-controls cell has a CSS rule")
	must(`class="icon-btn"`, "focus/hide render as icon buttons")
	must("eye-ico", "focus/hide use eye / slashed-eye SVG glyphs")
	must("disabled=${!member.online}", "focus/hide render disabled when the agent is offline")
	must("button:disabled", "disabled row buttons have a CSS rule")

	// One interaction provider owns mutual exclusion and dismissal. Keyed Preact
	// ownership lets snapshot refresh continue while a menu is open.
	must("const [openMenuKey, setOpenMenuKey] = useState('');", "the provider owns one open menu key")
	must("if (openMenuKeyRef.current === key) closeMenu(true);", "the native cog toggles its keyed menu")
	must("document.addEventListener('click', onClick);", "outside clicks dismiss native menus")
	must("menu.addEventListener('click', dismissItem);", "menu item clicks dismiss without detaching delegated targets")
	must("if (renameEditing) return true;",
		"only the unrelated legacy toolbar picker pauses the 2s poll")

	// Keyboard + ARIA: Escape closes an open menu, focus returns to the
	// owning cog, and the cog / menu / items carry the ARIA menu-button
	// roles. The menu also flips up when it would overflow the viewport.
	must(`aria-haspopup="menu"`, "the cog advertises its popup menu")
	must("aria-expanded=${open ? 'true' : 'false'}", "the cog keeps aria-expanded in sync")
	must(`role="menu"`, "the dropdown is an ARIA menu")
	must(`role="menuitem"`, "collected buttons are tagged role=menuitem")
	must("'Escape'", "Escape closes an open options menu")
	must("entry?.button?.focus()", "closing a menu restores focus to its owning cog")
	must(".action-menu.opens-up", "the menu can flip up to avoid viewport overflow")
	must("rect.bottom > window.innerHeight", "the native layout effect flips an overflowing menu")

	// Buttons that must STAY at the top level — never swallowed into a
	// menu. The group keeps spawn / power-on / shutdown; the agent row
	// keeps focus (jump) + hide.
	presentAction := func(act, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, `data-act="`+act+`"`) &&
			!strings.Contains(dashboardAssets, `act="`+act+`"`) {
			t.Errorf("dashboard source missing action %q (%s)", act, why)
		}
	}
	for _, act := range []string{
		"spawn-agent", "create-subgroup", "power-on-group", "shutdown-group",
	} {
		presentAction(act, "group header keeps "+act+" top-level")
	}
	must(`class="spawn-btn"`, "the group spawn button carries the .spawn-btn primary-CTA skin")
	must(`class="spawn-ico"`, "the group spawn button carries the user-plus icon")
	must(`class="subgroup-ico"`, "the subgroup button carries a portable two-person-plus icon")
	must(`openGroupCreateModal(undefined, group)`, "the subgroup shortcut opens group create pinned to its parent")
	must(`parent ? parentPrefill(null, parent)`,
		"switching a subgroup form back to blank restores the parent's editable defaults")
	must(`parentPrefill(template, parent)`,
		"selecting a template keeps the pinned parent authoritative and combines its startup context")
	must(`const sourceVisible = templateMode && !current.parentGroup;`,
		"a pinned subgroup hides the template mirror-source selector that cannot affect its inherited defaults")
	must(".spawn-btn {", "the .spawn-btn CSS rule ships with the dashboard — without it the chip falls back to bare browser styling")
	must("details[open] > summary .spawn-btn { opacity: 1; }",
		"spawn-btn fades with the rest of the group-action chips when the group is collapsed, brightens on hover / when open")
	for _, act := range []string{"jump", "hide"} {
		presentAction(act, "agent row keeps focus/hide ("+act+") top-level")
	}

	// Buttons that moved into a menu must still be rendered — relocated
	// in the DOM, not removed; their data-act is unchanged so the
	// existing dispatcher still handles them.
	// grant-owner / revoke-owner are the two arms of the owner-toggle
	// ternary — only one renders per row at runtime, and the native component
	// selects the action through its act prop.
	for _, act := range []string{
		"add-member", "cron-new", "message-new", "rename-group",
		"clone-group", "export-group", "cleanup-group", "window-modal-group",
		"delete-group", "term", "clone", "reincarnate", "edit-member",
		"perm-edit", "sudo-grant",
		"remove-member", "delete-agent",
	} {
		presentAction(act, act+" still rendered (moved into a menu)")
	}
	must("act=${member.owner ? 'revoke-owner' : 'grant-owner'}",
		"the owner toggle preserves both role actions in its native act prop")
}
