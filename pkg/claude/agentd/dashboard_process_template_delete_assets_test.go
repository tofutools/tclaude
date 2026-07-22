package agentd

import (
	"strings"
	"testing"
)

// TestDashboardJS_ProcessTemplateDeleteWired guards the two process-template
// delete affordances. Their behaviour is covered by
// jstest/process-template-delete.test.mjs; this guard — like the other
// dashboard-asset guards in this package — string-searches the embedded
// JS/HTML/CSS to pin the cross-module wiring that a component test mounting one
// island cannot see.
//
// The pieces that must stay connected:
//   - processes-island.js makes each template row draggable and gives it a
//     trash button routed at the shared deleteTemplate commit;
//   - process-template-dnd.js drives the drag with its own MIME and swaps the
//     shared bin into the template label voice;
//   - dnd.js explicitly ignores that MIME so the document-level drop handlers
//     stay isolated, exactly as it does for the group and dock drags;
//   - dashboard.html emits the template label voice for both vocab modes;
//   - the island binds and unbinds the drag module with its own lifetime.
func TestDashboardJS_ProcessTemplateDeleteWired(t *testing.T) {
	for _, c := range []struct{ needle, why string }{
		// processes-island.js: the row is the drag handle and carries the data
		// the drop commit needs without re-reading the list.
		{`data-process-template-drag=${template.id}`,
			"processes-island.js makes each template row the native draggable delete handle"},
		{`data-process-template-versions=${template.versionCount || 0}`,
			"the row carries its version count so the confirm can name what is lost"},
		{`data-process-action="delete"`, "each template row offers a click-to-delete button"},
		// process-template-dnd.js: dedicated MIME, label-voice swap, and the
		// interactive-child suppression that keeps the row's buttons clickable.
		{`'application/x-tclaude-process-template'`,
			"process-template-dnd.js uses a dedicated drag MIME"},
		{`const row = e.target.closest('[data-process-template-drag]');`,
			"process-template-dnd.js suppresses the row drag for clicks on interactive children"},
		{`trash.classList.add('show', 'dnd-trash-template-mode')`,
			"a template drag swaps the shared bin into its own label voice"},
		// dnd.js: the explicit isolation guard, matching the group/dock ones.
		{`e.dataTransfer.types.includes('application/x-tclaude-process-template')`,
			"dnd.js's drop handler explicitly ignores a process-template drop"},
		// dashboard.html: both vocab modes emitted for the template voice.
		{`<span class="dnd-trash-label-template">Delete</span>`,
			"the bin emits the plain template label"},
		{`<span class="dnd-trash-label-template-wizard">Unmake</span>`,
			"the bin emits the wizard template label"},
		// dashboard.css: wizard styling for the row delete button.
		{`body.wizard .process-list .process-actions .process-delete-btn`,
			"the row delete button has a wizard-mode skin"},
		// processes-island.js: the drag module is bound for the island's lifetime.
		{`const unbindTemplateDnd = bindProcessTemplateDnd();`,
			"the Processes island binds the template drag module"},
		{`registerCleanup(() => { unbindTemplateDnd(); setProcessTemplateDeleteHandler(null); });`,
			"the island unbinds the drag module and drops the handler on teardown"},
		// dragend bubbles, so the document handler must self-gate like the rest.
		{`listen(document, 'dragend', () => { if (templateDragActive) endTemplateDrag(); });`,
			"the dragend handler self-gates so a foreign drag cannot clear the shared bin"},
		// processes-actions.js: one shared commit behind both affordances.
		{`async function deleteTemplate({ id, name = '', versionCount = 0 } = {}) {`,
			"both delete affordances route through one commit"},
	} {
		if !strings.Contains(dashboardAssets, c.needle) {
			t.Errorf("dashboard assets missing %q — %s", c.needle, c.why)
		}
	}
}
