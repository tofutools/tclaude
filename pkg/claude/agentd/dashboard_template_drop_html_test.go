package agentd

import (
	"strings"
	"testing"
)

// JOH-377 4/4: dragging a palette-dock TEMPLATE card onto a group opens the
// unified summon dialog with a drop-mode chooser — reinforce the group in place
// (POST …/reinforce), create a NEW top-level group in its image, or create a
// NEW subgroup in its image (POST …/instantiate carrying the JOH-356
// context_override and, for subgroup, parent). Dropping onto empty space opens
// the plain "new party from circle" flow with optional mirror settings. Like
// the other dashboard render guards
// this pins the wiring across HTML / CSS / JS by string-searching the embedded
// source rather than running the JS, so a rename that silently breaks the drag
// in the browser fails at `go test ./...` instead. It is a pure-frontend feature
// wiring EXISTING endpoints (no new endpoint, no schema), so this string pin is
// the coverage; the reinforce backend itself is covered by reinforce_flow_test.
func TestDashboardHTML_TemplateDrop(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}
	mustOrder := func(first, second, why string) {
		t.Helper()
		source := compactDashboardSource(dashboardAssets)
		i := strings.Index(source, compactDashboardSource(first))
		j := strings.Index(source, compactDashboardSource(second))
		if i < 0 || j < 0 || i >= j {
			t.Errorf("dashboard source order wrong (%s): %q should appear before %q", why, first, second)
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

	// The mode chooser markup — the three radio options, both vocab modes.
	must(`id="template-deploy-mode"`, "the drop-mode chooser exists")
	must(`name="template-deploy-mode" value=${value} checked=${mode === value}`, "the controlled native mode radios exist")
	must("[['subgroup', 'New subgroup copying this group’s settings'", "the subgroup mode is the default/first option")
	mustOrder("['subgroup', 'New subgroup copying this group’s settings'", "['reinforce', 'Reinforce this group'",
		"subgroup is the first drop-mode option")
	must(`['reinforce', 'Reinforce this group', 'Summon into this party']`,
		"the reinforce option ships both voices")
	must(`'copy', 'New top-level group copying settings', 'New top-level party in this party’s image'`,
		"the copy option ships both voices")
	must(`'subgroup', 'New subgroup copying this group’s settings', 'New sub-party in this party’s image'`,
		"the subgroup option ships both voices")

	// The mirror/copy-mode fields + the per-mode note.
	must(`id="template-deploy-descr-row"`, "the copy-mode description row exists")
	must(`id="template-deploy-descr"`, "the copy-mode description input exists")
	must(`id="template-deploy-context-row"`, "the copy-mode context row exists")
	must(`id="template-deploy-context"`, "the copy-mode context textarea exists")
	must(`id="template-deploy-source-row"`, "the normal-open mirror-source row exists")
	must(`id="template-deploy-parent-row"`, "the normal-open subgroup checkbox row exists")
	must(`id="template-deploy-group-note"`, "the per-mode group note exists")

	// One submit handler, mode-dispatched — reinforce and copy fork to their own
	// POSTs; the create-new path is unchanged.
	must("actions.deployTemplate(templateName, 'reinforce', payload, mode)", "reinforce mode dispatches to the reinforce POST")
	must("await actions.deployTemplate(", "copy/subgroup modes use the shared deploy action")
	must("'instantiate', payload, mode,", "copy/subgroup modes dispatch to the instantiate POST")
	must("kind === 'reinforce' ? 'reinforce' : kind === 'instantiate' ? 'instantiate' : 'deploy'", "the API action selects the existing endpoints")
	// Copy mode ALWAYS sends context_override AND descr_override so the new group
	// carries the visible, edited combined context (JOH-356) AND description
	// (JOH-385). Sending them unconditionally is how an EMPTY source context /
	// description is faithfully copied (a bare `descr` would let the backend
	// re-default an empty description to "Instantiated from template X").
	must("combineContext(", "the visible context combines the mirrored group and template defaults")
	must("## Mirrored group context", "the combined context labels the mirrored group portion")
	must("## Template context", "the combined context labels the template portion")
	must("const payload = {", "copy mode constructs an explicit payload")
	must("group_name: groupName.trim(),", "copy mode sends the visible group name")
	must("context_override: context,", "copy mode sends the visible combined context")
	must("descr_override: descr.trim(),", "copy mode sends the visible description")
	must("if (mode === 'subgroup') payload.parent = dropGroup.name;", "subgroup drop mode sends the parent group")
	must("payload.descr_override = descr.trim();", "normal deploy can mirror a group's description")
	must("payload.context_override = context;", "normal deploy can mirror a group's context")
	must("if (parent) payload.parent = source;", "normal deploy can nest under the mirrored source")

	// The chooser reflows the dialog live (no re-open) — the mode-radio change
	// listener + the locked/prefilled group field.
	must("onChange=${() => setMode(value)}", "switching mode reflows the dialog live")
	must("readonly=${reinforcing}", "reinforce mode locks the group name to the target")
	must("class=${reinforcing ? 'locked' : ''}", "reinforce mode visibly marks the locked group name")
	must("useState(dropGroup ? 'subgroup' : '')", "the JS default is subgroup for group drops")

	// CSS: the chooser is styled in both skins, wizard SCOPED under the modal (the
	// anti-pin invariant — no unscoped body.wizard widening).
	must(".deploy-mode {", "the drop-mode chooser has a base CSS rule")
	must("body.wizard #template-deploy-modal .deploy-mode {", "the wizard chooser skin is scoped to the summon modal")
	must("#template-deploy-group.locked {", "the locked group field is visibly muted")

	// The chooser + copy-only rows must actually hide when marked `hidden` on a
	// NORMAL open (the normal-open invariant). Their author `display:flex` beats
	// the UA `[hidden]{display:none}`, so JOH-383 added ONE global author-origin
	// `[hidden] { display: none !important }` rule that makes the attribute win
	// over any author display for every element at once — retiring the per-class
	// .deploy-mode[hidden] / .cron-create-row[hidden] overrides this used to pin.
	// String tests can't see the cascade, so pin the global rule that enforces it.
	must("[hidden] { display: none !important; }", "the global rule makes the `hidden` attribute win over any author display, so the chooser + copy-only rows hide on a normal open (JOH-383)")
}
