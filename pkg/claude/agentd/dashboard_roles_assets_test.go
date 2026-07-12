package agentd

import (
	"strings"
	"testing"
)

// TestDashboardAssets_RoleLibraryWired guards the role-library dashboard
// (JOH-240), whose pieces span several files that must stay in lockstep —
// there's no JS render test, so we assert on the embedded concatenation at
// `go test ./...`. A rename in any one file silently breaks the feature only
// in the browser.
func TestDashboardAssets_RoleLibraryWired(t *testing.T) {
	for _, needle := range []string{
		// roles.js — the data layer's endpoint + exports.
		"const API = '/api/roles';",
		"loadRoles, cachedRoles, invalidateRoles,",
		// modal-roles.js — compatibility entry points into the Preact owner.
		"function bindRolesUI(",
		"function openRolesManageModal(",
		"function openRoleEditor(",
		// dashboard.js — the editor is bound at init.
		"bindRolesUI();",
		"import { bindRolesUI } from './modal-roles.js';",
		// palette.js — the command-palette entry.
		"run: () => openRolesManageModal(),",
		// modal-templates.js — the per-agent role dropdown reads/writes role_ref.
		"function roleRefOptionsHTML(",
		`<select class="ta-role-ref">`,
		"role_ref: $('.ta-role-ref', row).value.trim(),",
		// role-inspect.js — the shared transparency panel (JOH-351) + its wiring
		// into the templates role dropdown. A rename in any file silently drops
		// the inspect affordance in the browser only.
		"function roleInspectHTML(",
		"import { roleInspectHTML } from './role-inspect.js';",
		"function roleInspectFor(",
		`<div class="ta-role-inspect">`,
		// dashboard.css — the inspect panel's dark-theme styling.
		".role-inspect {",
		// dashboard.html + management-island.js — the Groups-cog entry, root,
		// and Preact-owned modal ids.
		`id="roles-manage-open"`,
		`id="management-root"`,
		"id=${`${domKind}-manage-modal`}",
		`id="role-editor-modal"`,
		`id="role-editor-brief"`,
		`id="role-editor-submit"`,
		`.cron-create-row input:not([type])`,
		`body.wizard #role-editor-modal .cron-create-row input:not([type])`,
		`id="profile-editor-perms" class="tool"`,
		// dashboard.css — the pure-CSS wizard vocabulary swap ("roles" → "classes").
		"body.wizard .roles-word-wizard { display: inline; }",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — role-library wiring broken", needle)
		}
	}
}
