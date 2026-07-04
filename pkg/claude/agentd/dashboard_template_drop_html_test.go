package agentd

import (
	"strings"
	"testing"
)

// JOH-377 4/4: dragging a palette-dock TEMPLATE card onto a group opens the
// unified summon dialog with a drop-mode chooser — reinforce the group in place
// (POST …/reinforce) or create a NEW group in its image (POST …/instantiate
// carrying the JOH-356 context_override). Dropping onto empty space opens the
// plain "new party from circle" flow. Like the other dashboard render guards
// this pins the wiring across HTML / CSS / JS by string-searching the embedded
// source rather than running the JS, so a rename that silently breaks the drag
// in the browser fails at `go test ./...` instead. It is a pure-frontend feature
// wiring EXISTING endpoints (no new endpoint, no schema), so this string pin is
// the coverage; the reinforce backend itself is covered by reinforce_flow_test.
func TestDashboardHTML_TemplateDrop(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Templates are now a drag SOURCE alongside profiles + roles.
	must("new Set(['profiles', 'roles', 'templates'])", "templates joined the draggable dock kinds")

	// The drop dispatch: a template drop opens the unified summon dialog via the
	// exported opener; dock-dnd imports it from modal-templates.
	must("import { openSummonForDrop } from './modal-templates.js';", "dock-dnd imports the template-drop opener")
	must("if (item.kind === 'templates') {", "the drop handler forks templates to the summon dialog")
	must("openSummonForDrop(item.name, group)", "a template drop opens the summon dialog with the drop target")
	must("export function openSummonForDrop(", "modal-templates exports the template-drop opener")

	// The hover pill gets a template-specific hint (deploy → group / new party
	// from, in both skins).
	must("wizWord('deploy', 'summon')", "the pill reads deploy/summon for a template onto a group")
	must("wizWord('new group from', 'new party from')", "the pill reads new-group for a template onto empty space")

	// The mode chooser markup — the two radio options, both vocab modes.
	must(`id="template-deploy-mode"`, "the drop-mode chooser exists")
	must(`name="template-deploy-mode" value="reinforce"`, "the reinforce mode radio exists")
	must(`name="template-deploy-mode" value="copy"`, "the copy mode radio exists")
	must(`<span class="tpl-word-regular">Reinforce this group</span><span class="tpl-word-wizard">Summon into this party</span>`,
		"the reinforce option ships both voices")
	must(`<span class="tpl-word-regular">New group copying this group's settings</span><span class="tpl-word-wizard">New party in this party's image</span>`,
		"the copy option ships both voices")

	// The copy-mode-only fields (hidden on a normal open) + the per-mode note.
	must(`id="template-deploy-descr-row"`, "the copy-mode description row exists")
	must(`id="template-deploy-descr"`, "the copy-mode description input exists")
	must(`id="template-deploy-context-row"`, "the copy-mode context row exists")
	must(`id="template-deploy-context"`, "the copy-mode context textarea exists")
	must(`id="template-deploy-group-note"`, "the per-mode group note exists")

	// One submit handler, mode-dispatched — reinforce and copy fork to their own
	// POSTs; the create-new path is unchanged.
	must("if (deployDropGroup && deployMode() === 'reinforce') return submitReinforce();", "reinforce mode dispatches to the reinforce POST")
	must("if (deployDropGroup && deployMode() === 'copy') return submitCopyGroup();", "copy mode dispatches to the instantiate POST")
	must("/reinforce`, {", "reinforce mode POSTs to the reinforce endpoint")
	must("/instantiate`, {", "copy mode POSTs to the existing instantiate endpoint")
	// Copy mode ALWAYS sends context_override so the new group carries the target's
	// context (JOH-356), never the template's.
	must("const payload = { group_name: groupName, context_override: context };", "copy mode overrides the context with the group's own copy")

	// The chooser reflows the dialog live (no re-open) — the mode-radio change
	// listener + the locked/prefilled group field.
	must("rdo.addEventListener('change', applyDeployMode)", "switching mode reflows the dialog live")
	must("groupInput.readOnly = true;", "reinforce mode locks the group name to the target")

	// CSS: the chooser is styled in both skins, wizard SCOPED under the modal (the
	// anti-pin invariant — no unscoped body.wizard widening).
	must(".deploy-mode {", "the drop-mode chooser has a base CSS rule")
	must("body.wizard #template-deploy-modal .deploy-mode {", "the wizard chooser skin is scoped to the summon modal")
	must("#template-deploy-group.locked {", "the locked group field is visibly muted")

	// The [hidden] overrides are load-bearing: the author `display:flex` on
	// .deploy-mode / .cron-create-row beats the UA `[hidden]{display:none}`, so
	// without these the chooser + copy-only rows show on a NORMAL open (the
	// normal-open invariant). String tests can't see the cascade, so pin the rules.
	must(".deploy-mode[hidden] { display: none; }", "the chooser's hidden attr actually hides (author display beats UA [hidden])")
	must(".cron-create-row[hidden] { display: none; }", "the copy-only + worktree rows' hidden attr actually hides")
}
