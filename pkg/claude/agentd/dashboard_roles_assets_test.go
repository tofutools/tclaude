package agentd

import "testing"

// TestDashboardAssets_RoleLibraryWired guards the role-library dashboard
// (JOH-240), whose pieces span several files that must stay in lockstep. The
// Preact render behaviour has a JS DOM test; these asset assertions pin the
// surrounding entry points and the controlled editor wiring.
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
		// template-management-island/model — the controlled per-agent role
		// dropdown reads/writes role_ref and renders its transparency panel.
		`<select class="ta-role-ref" value=${agent.role_ref}`,
		"role_ref: agent.role_ref.trim(),",
		"function RoleInspect({ roleName, roles })",
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
		`id="role-editor-perms" class="tool"`,
		`id="role-editor-submit"`,
		`id="agent-spawn-role-refs"`,
		`id="profile-editor-role-refs"`,
		`role_refs: [...new Set((draft.roleRefs || [])`,
		`.cron-create-row input:not([type])`,
		`body.wizard #role-editor-modal .cron-create-row input:not([type])`,
		`id="profile-editor-perms" class="tool"`,
		`.cron-create-row > button.tool:is(#profile-editor-perms, #profile-editor-context-features, #role-editor-perms)`,
		// dashboard.css — the pure-CSS wizard vocabulary swap ("roles" → "classes").
		"body.wizard .roles-word-wizard { display: inline; }",
	} {
		if !dashboardSourceContains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — role-library wiring broken", needle)
		}
	}
	for _, obsolete := range []string{
		`id="role-editor-harness"`, `id="role-editor-model"`,
		`id="role-editor-sandbox"`, `id="role-editor-approval"`,
	} {
		if dashboardSourceContains(dashboardAssets, obsolete) {
			t.Errorf("dashboard assets still expose role-owned launch control %q", obsolete)
		}
	}
}
