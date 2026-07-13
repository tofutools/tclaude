package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_TemplateCwdBrowse pins the "Browse…" native directory
// picker wired beside the cwd field of the unified summon/deploy dialog
// (#template-deploy-cwd) (JOH-372, JOH-373). It reuses the group-create /
// agent-spawn idiom: a .dir-browse-btn button right after the input, wired in
// bindTemplatesUI to the shared pickDirectory helper. A rename of any of these
// ids — or a dropped wiring call / import — breaks the picker silently, so it
// is pinned.
//
// The retired instantiate/cast dialog (#template-instantiate-cwd-browse) was
// folded into this one (JOH-373), so only the unified dialog's picker remains.
func TestDashboardHTML_TemplateCwdBrowse(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The unified dialog's Browse… button, byte-identical to the group-create
	// idiom bar the id, sits right after its cwd input.
	must(`<button id="template-deploy-cwd-browse" type="button" class="dir-browse-btn" title="Open a native directory picker on the daemon's desktop"`,
		"the summon dialog gains a Browse… button beside its cwd field")

	// The Preact dialog imports the shared helper and its two buttons call the
	// same controlled-state browse action with the appropriate destination.
	must("import { pickDirectory } from './helpers.js';", "the Preact template dialog imports pickDirectory")
	must("onClick=${() => browse('cwd')}", "the cwd Browse… button is wired")
	must("onClick=${() => browse('repo')}", "the worktree-repo Browse… button is wired")
	must("setCwd(result.path)", "a picked cwd updates controlled state")
	must("setRepo(result.path)", "a picked worktree repo updates controlled state")

	// The folded-in cast dialog's picker id is gone — guard against a stray
	// re-introduction of the second dialog.
	if strings.Contains(dashboardAssets, "template-instantiate-cwd-browse") {
		t.Error("dashboard still references template-instantiate-cwd-browse — the cast dialog was folded into the unified summon dialog (JOH-373)")
	}
}
