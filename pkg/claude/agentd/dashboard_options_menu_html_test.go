package agentd

import (
	"strings"
	"testing"
)

// The dashboard's Groups tab collapses each agent row's and each group
// header's less-used buttons behind a ⚙ "options" cog: clicking the cog
// opens a small .action-menu of the collected actions. This is entirely
// client-side JS (helpers.js / render.js / row-actions.js / refresh.js)
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

	// The cog + collapsed-menu wiring. The cog's data-act is templated
	// (`data-act="${esc(act)}"`), so the row-menu / group-menu literals
	// appear at the actionCog(...) call sites, not as a static attr.
	must(`class="cog-btn"`, "the ⚙ cog renders as a .cog-btn button")
	must("actionCog('row-menu',", "agent rows carry a ⚙ options cog (row-menu)")
	must("actionCog('group-menu',", "group headers carry a ⚙ options cog (group-menu)")
	must(`class="action-menu"`, "the collected buttons live in a .action-menu")
	must(".action-menu {", "the .action-menu dropdown has a CSS rule")
	must(".action-menu.open", "an open menu is shown via the .open class")

	// The cog toggle is dispatched in row-actions.js, closes any other
	// open menu, and the auto-refresh is suspended while a menu is open.
	must("function closeAllActionMenus(",
		"row-actions.js closes menus on outside / item clicks")
	must("case 'row-menu':", "row-actions.js dispatches the agent-row cog")
	must("case 'group-menu':", "row-actions.js dispatches the group cog")
	must("querySelector('.action-menu.open')",
		"refreshSuspended() pauses the 5s poll while a menu is open")

	// Buttons that must STAY at the top level — never swallowed into a
	// menu. The group keeps spawn / power-on / shutdown; the agent row
	// keeps focus (jump) + hide.
	for _, act := range []string{
		"spawn-agent", "power-on-group", "shutdown-group",
	} {
		must(`data-act="`+act+`"`, "group header keeps "+act+" top-level")
	}
	must(">+ spawn</button>", "the group spawn button is relabelled to 'spawn'")
	for _, act := range []string{"jump", "hide"} {
		must(`data-act="`+act+`"`, "agent row keeps focus/hide ("+act+") top-level")
	}

	// Buttons that moved into a menu must still be rendered — relocated
	// in the DOM, not removed; their data-act is unchanged so the
	// existing dispatcher still handles them.
	for _, act := range []string{
		"add-member", "cron-new", "message-new", "rename-group",
		"export-group", "cleanup-group", "window-modal-group",
		"delete-group", "term", "clone", "reincarnate", "edit-member",
		"perm-edit", "sudo-grant", "remove-member", "delete-agent",
	} {
		must(`data-act="`+act+`"`, act+" still rendered (moved into a menu)")
	}
}
