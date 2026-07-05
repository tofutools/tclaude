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
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The unified dialog's Browse… button, byte-identical to the group-create
	// idiom bar the id, sits right after its cwd input.
	must(`<button id="template-deploy-cwd-browse" type="button" class="dir-browse-btn" title="Open a native directory picker on the daemon's desktop">Browse…</button>`,
		"the summon dialog gains a Browse… button beside its cwd field")

	// modal-templates.js imports the shared helper and wires the button.
	// (Needle tracks the file's literal import line — bindModalSubmitHotkey
	// joined it for the editor's Ctrl/Cmd+Enter save.)
	must("import { $, $$, esc, makeModalResizable, bindModalSubmitHotkey, pickDirectory } from './helpers.js';",
		"modal-templates.js imports pickDirectory from helpers.js")
	must(`wireTemplateCwdBrowse('template-deploy-cwd-browse', 'template-deploy-cwd', 'template-deploy-error', 'Select the working directory for the task force');`,
		"the summon dialog's Browse… is wired in bindTemplatesUI")
	must(`wireTemplateCwdBrowse('template-deploy-wt-repo-browse', 'template-deploy-wt-repo', 'template-deploy-error', 'Select the git repo to worktree');`,
		"the summon dialog's Worktree repo Browse… is wired in bindTemplatesUI")
	must("input.dispatchEvent(new Event('input', { bubbles: true }));",
		"template browse picks dispatch input so cwd/worktree reload listeners run")

	// The folded-in cast dialog's picker id is gone — guard against a stray
	// re-introduction of the second dialog.
	if strings.Contains(dashboardAssets, "template-instantiate-cwd-browse") {
		t.Error("dashboard still references template-instantiate-cwd-browse — the cast dialog was folded into the unified summon dialog (JOH-373)")
	}
}
