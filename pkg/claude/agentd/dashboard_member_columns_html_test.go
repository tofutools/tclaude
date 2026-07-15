package agentd

import (
	"strings"
	"testing"
)

// The Groups tab's per-column show/hide feature is split across four files
// that have to stay in lockstep — a rename in any one silently breaks it in
// the browser, where no Go path exercises it. The store logic itself is
// unit-tested in jstest/member-columns.test.mjs; this guards that the WIRING
// (the single-source-of-truth column model, the header/body alignment, the
// ▾ view "Columns" menu, and its theming) survives a refactor of the
// embedded assets. Asserting on the embedded concatenation catches a drop at
// `go test ./...`.
func TestDashboardHTML_MemberColumnsShowHideWired(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q (%s)", needle, why)
		}
	}

	// member-columns.js: the store the header, body and menu all read.
	must("function memberColHidden(", "effective per-column hidden state")
	must("function setMemberColHidden(", "persist a column's hidden state")
	must("function visibleMemberCols(", "the ordered visible-column list")
	must("function memberColDeviationCount(", "badge count of non-default columns")
	must("'tclaude.dash.members.hidden'", "hidden set persists under a stable dashPrefs key")

	// groups-member-table.js: the native header AND body render off the SAME
	// visible-column list, so they can never drift out of alignment.
	must("import { visibleMemberCols, memberColHidden } from './member-columns.js';", "render imports the store")
	must("const columns = visibleMemberCols();", "the native member table resolves visible columns once")
	must(`<${SortHead} table="members" columns=${columns}`, "the members header receives that visible-column list")
	must("wizardLabel: 'Class'", "the Role header and column toggle use wizard Class vocabulary")
	must("wizardLabel: 'Quest'", "the Task link header and column toggle use wizard Quest vocabulary")
	must("wizardLabel: 'Lore'", "the Description header and column toggle use wizard Lore vocabulary")
	must("columns.map((column) => html`<${MemberCell}", "each row emits only the visible columns, in order")
	// A missing cell must degrade to an empty <td> (fails ALIGNED), never to
	// '' (which would shift every later cell left into a misaligned table).
	must("default: return html`<td></td>`", "a visible column with no cell keeps the row aligned")
	// Hiding ID folds its agent-id/conv-id hover onto the Name cell.
	must("const idPrefix = memberColHidden('id')", "the name cell checks whether ID is hidden")
	must("`${idTooltip(member.agent_id, member.conv_id)} — `", "the hidden ID pair is folded into the name tooltip")

	// groups-state/island: the ▾ view "Columns" section is generated from the
	// column model and each toggle persists + rerenders + feeds the badge.
	must("list: hideableMemberCols", "the menu is built from the hideable columns")
	must("setHidden: setMemberColHidden", "a column toggle persists its new state")
	must("filter-groups-col-${column.key}", "each column checkbox gets a stable id")
	must("columns.deviationCount()", "the view badge counts hidden columns too")

	// groups-island.js: the menu section + generated checkbox container.
	must(`id="filter-groups-cols"`, "the Columns checkbox container is present")
	must(`class="view-menu-heading"`, "the Columns section has a heading")
	must(`class="view-menu-sep"`, "a divider separates row toggles from column toggles")

	// dashboard.css: the section is styled in both the default and wizard
	// themes (the operator asked wizard mode not be left unstyled).
	must(".view-menu .view-cols", "the column toggles have a layout rule")
	must(".view-menu .view-menu-heading", "the Columns heading is styled")
	must("body.wizard .view-menu .view-menu-heading", "wizard mode themes the Columns heading")
	must("body.wizard .view-menu .view-menu-sep", "wizard mode themes the section divider")
}
