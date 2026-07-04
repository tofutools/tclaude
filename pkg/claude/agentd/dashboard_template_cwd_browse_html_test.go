package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_TemplateCwdBrowse pins the "Browse…" native directory
// picker wired beside the cwd fields of the cast/instantiate
// (#template-instantiate-cwd) and summon/deploy (#template-deploy-cwd)
// dialogs (JOH-372). Both reuse the group-create / agent-spawn idiom: a
// .dir-browse-btn button right after the input, wired in bindTemplatesUI to
// the shared pickDirectory helper. A rename of any of these ids — or a
// dropped wiring call / import — breaks the picker silently, so it is pinned.
func TestDashboardHTML_TemplateCwdBrowse(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The two Browse… buttons, byte-identical to the group-create idiom
	// bar the id, sit right after their cwd inputs.
	must(`<button id="template-instantiate-cwd-browse" type="button" class="dir-browse-btn" title="Open a native directory picker on the daemon's desktop">Browse…</button>`,
		"the cast dialog gains a Browse… button beside its cwd field")
	must(`<button id="template-deploy-cwd-browse" type="button" class="dir-browse-btn" title="Open a native directory picker on the daemon's desktop">Browse…</button>`,
		"the summon dialog gains a Browse… button beside its cwd field")

	// modal-templates.js imports the shared helper and wires both buttons.
	must("import { $, $$, esc, makeModalResizable, pickDirectory } from './helpers.js';",
		"modal-templates.js imports pickDirectory from helpers.js")
	must(`wireTemplateCwdBrowse('template-instantiate-cwd-browse', 'template-instantiate-cwd', 'template-instantiate-error', 'Select the working directory for the new group');`,
		"the cast dialog's Browse… is wired in bindTemplatesUI")
	must(`wireTemplateCwdBrowse('template-deploy-cwd-browse', 'template-deploy-cwd', 'template-deploy-error', 'Select the working directory for the task force');`,
		"the summon dialog's Browse… is wired in bindTemplatesUI")
}
