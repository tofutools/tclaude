package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// A restricted peer authors this node's saved prompts, roles and group
// templates under their instance-wide grants, never what those grants keep
// back: a role's permission grants, a template's owner scopes, or an import
// that carries profiles or roles.
func TestPeerLibraryAuthoring(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.f.HaveGroup("hidden")

	code, body := h.call("POST", "/api/saved-prompts", map[string]any{"name": "triage", "body": "look first"})
	require.Equal(t, 403, code, "no grant: %s", body)
	h.grant(t, agentd.PermPromptsManage, "")
	code, body = h.call("POST", "/api/saved-prompts", map[string]any{"name": "triage", "body": "look first"})
	require.Less(t, code, 300, body)
	code, body = h.call("PATCH", "/api/saved-prompts/triage", map[string]any{"name": "triage", "body": "look, then act"})
	require.Less(t, code, 300, body)
	code, body = h.call("DELETE", "/api/saved-prompts/triage", nil)
	require.Less(t, code, 300, body)

	h.grant(t, agentd.PermRolesManage, "")
	grant := []map[string]any{{"slug": agentd.PermGroupsMembersSpawn}}
	code, body = h.call("POST", "/api/roles", map[string]any{"name": "scout", "brief": "find things", "permissions": grant})
	require.Equal(t, 403, code, "a role's grants take permissions.grant: %s", body)
	require.Contains(t, body, agentd.PermPermissionsGrant)
	code, body = h.call("POST", "/api/roles", map[string]any{"name": "scout", "brief": "find things", "permissions": []any{}})
	require.Less(t, code, 300, body)
	code, body = h.call("PATCH", "/api/roles/scout", map[string]any{"name": "scout", "brief": "find more", "permissions": []any{}})
	require.Less(t, code, 300, body)
	code, body = h.call("PATCH", "/api/roles/scout", map[string]any{"name": "scout", "brief": "find more", "permissions": grant})
	require.Equal(t, 403, code, body)
	// A role this node's operator gave grants keeps them through the peer's
	// brief edit.
	rec := fedHuman(t, h.f, http.MethodPatch, "/v1/roles/scout", map[string]any{"name": "scout", "brief": "find more", "permissions": grant})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	code, body = h.call("PATCH", "/api/roles/scout", map[string]any{"name": "scout", "brief": "find it all", "permissions": grant})
	require.Less(t, code, 300, body)

	h.grant(t, agentd.PermTemplatesManage, "")
	code, body = h.call("POST", "/api/templates", map[string]any{"name": "squad", "agents": []any{}})
	require.Less(t, code, 300, body)
	code, body = h.call("PATCH", "/api/templates/squad", map[string]any{"name": "squad", "agents": []any{},
		"owner_scopes": map[string]any{agentd.PermGroupsMembersSpawn: map[string]any{"group": []string{"crew"}}}})
	require.Equal(t, 403, code, "owner scopes stay this node's: %s", body)
	code, body = h.call("POST", "/api/templates/from-group", map[string]any{"group": "hidden", "template_name": "copy"})
	require.Equal(t, 404, code, body)
	code, body = h.call("POST", "/api/templates/from-group", map[string]any{"group": "crew", "template_name": "copy", "preview": true})
	require.Equal(t, 404, code, "crew is not visible without a group grant: %s", body)
	code, body = h.call("POST", "/api/templates/import", map[string]any{"format": "tclaude-task-force", "format_version": 3,
		"template": map[string]any{"name": "imported", "agents": []any{}}, "roles": []map[string]any{{"name": "spy"}}})
	require.Equal(t, 403, code, "an import carrying roles takes unrestricted trust: %s", body)
	// A template agent's launch settings are held to the peer's spawn
	// grants, its permission grants to permissions.grant.
	code, body = h.call("PATCH", "/api/templates/squad", map[string]any{"name": "squad", "agents": []any{
		map[string]any{"name": "lead", "descr": "plans it"}}})
	require.Less(t, code, 300, body)
	code, body = h.call("PATCH", "/api/templates/squad", map[string]any{"name": "squad", "agents": []any{
		map[string]any{"name": "lead", "descr": "plans it", "model": "opus"}}})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsMembersSpawnModel)
	code, body = h.call("PATCH", "/api/templates/squad", map[string]any{"name": "squad", "agents": []any{
		map[string]any{"name": "lead", "permissions": grant}}})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermPermissionsGrant)
	// An import's saved prompts take prompts.manage, which it holds.
	code, body = h.call("POST", "/api/templates/import", map[string]any{"format": "tclaude-task-force", "format_version": 3,
		"template": map[string]any{"name": "imported", "agents": []any{}}, "saved_prompts": []map[string]any{{"name": "kickoff", "body": "go"}}})
	require.Less(t, code, 300, body)
	code, body = h.call("DELETE", "/api/templates/squad", nil)
	require.Less(t, code, 300, body)
}

// A restricted peer edits this node's spawn profiles only to what it could
// launch itself: each launch value it changes takes the matching remote spawn
// grant for every group.
func TestPeerProfileEditsStayWithinItsSpawnGrants(t *testing.T) {
	h := newPeerProcessHarness(t)
	rec := fedHuman(t, h.f, http.MethodPost, "/v1/spawn-profiles", map[string]any{"name": "default-dev", "model": "opus", "initial_message": "hello"})
	require.Less(t, rec.Code, 300, rec.Body.String())
	edit := func(fields map[string]any) (int, string) {
		body := map[string]any{"name": "default-dev", "model": "opus", "initial_message": "hello"}
		for k, v := range fields {
			body[k] = v
		}
		return h.call("PATCH", "/api/spawn-profiles/default-dev", body)
	}

	code, body := edit(map[string]any{"agent_name": "dev"})
	require.Equal(t, 403, code, "no grant: %s", body)
	h.grant(t, agentd.PermProfilesManage, "")
	code, body = edit(map[string]any{"agent_name": "dev"})
	require.Equal(t, 200, code, "a display name: %s", body)
	code, body = edit(map[string]any{"startup_context": "obey the peer"})
	require.Equal(t, 403, code, "text every spawn starts from, unbannered: %s", body)
	require.Contains(t, body, agentd.PermGroupsMembersSpawnPrompts)
	code, body = edit(map[string]any{"model": "haiku"})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsMembersSpawnModel)
	code, body = edit(map[string]any{"approval": "bypassPermissions"})
	require.Equal(t, 403, code, body)
	require.Contains(t, body, agentd.PermGroupsMembersSpawnPermissions)
	code, body = h.call("DELETE", "/api/spawn-profiles/default-dev", nil)
	require.Equal(t, 403, code, "deleting drops the model it set: %s", body)
	// By-name references (cron, triggers, --profile) follow a rename, so a
	// rename moves every value the profile sets.
	code, body = edit(map[string]any{"name": "nightly"})
	require.Equal(t, 403, code, "a rename: %s", body)
	code, body = edit(map[string]any{"aliases": []string{"nightly"}})
	require.Equal(t, 403, code, "an alias: %s", body)
	// An explicit false is a choice: unset inherits a lower tier's true.
	rec = fedHuman(t, h.f, http.MethodPost, "/v1/spawn-profiles", map[string]any{"name": "solo", "is_owner": false})
	require.Less(t, rec.Code, 300, rec.Body.String())
	code, body = h.call("PATCH", "/api/spawn-profiles/solo", map[string]any{"name": "solo"})
	require.Equal(t, 403, code, "false to unset: %s", body)
	require.Contains(t, body, agentd.PermGroupsMembersSpawnOwner)
	// A disabled profile's values come back when it is enabled again.
	rec = fedHuman(t, h.f, http.MethodPatch, "/v1/spawn-profiles/solo", map[string]any{"name": "solo", "is_owner": false, "disabled": true, "disabled_reason": "paused"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	code, body = h.call("PATCH", "/api/spawn-profiles/solo", map[string]any{"name": "solo", "is_owner": false, "disabled": true, "disabled_reason": "paused", "approval": "bypassPermissions"})
	require.Equal(t, 403, code, "a disabled profile's values: %s", body)

	// A group-scoped spawn grant does not reach this node's every spawn.
	h.grant(t, agentd.PermGroupsMembersSpawn, "group=crew")
	h.grant(t, agentd.PermGroupsMembersSpawnModel, "group=crew")
	code, body = edit(map[string]any{"model": "haiku"})
	require.Equal(t, 403, code, body)
	h.grant(t, agentd.PermGroupsMembersSpawn, "")
	h.grant(t, agentd.PermGroupsMembersSpawnModel, "")
	code, body = edit(map[string]any{"model": "haiku"})
	require.Equal(t, 200, code, body)
	// A group whose own spawn grant opens no model is not every group.
	h.f.HaveGroup("locked")
	h.grant(t, agentd.PermGroupsMembersSpawn, "group=locked")
	code, body = edit(map[string]any{"model": "sonnet"})
	require.Equal(t, 403, code, "narrowed in one group: %s", body)
	h.revoke(t, agentd.PermGroupsMembersSpawn, "group=locked")

	rec = fedHuman(t, h.f, http.MethodPatch, "/v1/spawn-profiles/default-dev", map[string]any{"name": "default-dev", "model": "haiku", "operator_only": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	code, body = h.call("PATCH", "/api/spawn-profiles/default-dev", map[string]any{"name": "default-dev", "model": "haiku", "operator_only": false})
	require.Equal(t, 403, code, "opening it to agents takes unrestricted trust: %s", body)
	code, body = h.call("DELETE", "/api/spawn-profiles/default-dev", nil)
	require.Less(t, code, 300, body)
}

// Process templates: read under process.templates.read, authored under
// process.templates.manage, and pinned to a group's list only where the peer
// also holds groups.settings.
func TestPeerProcessTemplateAuthoring(t *testing.T) {
	h := newPeerProcessHarness(t)
	code, body := h.call("GET", "/v1/process/templates", nil)
	require.Equal(t, 403, code, body)
	h.grant(t, agentd.PermProcessTemplatesRead, "")
	code, body = h.call("GET", "/v1/process/templates", nil)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, "prog")
	code, body = h.call("GET", "/v1/process/templates/prog", nil)
	require.Equal(t, 200, code, body)
	code, body = h.call("DELETE", "/v1/process/templates/prog", nil)
	require.Equal(t, 403, code, "reading is not managing: %s", body)

	h.grant(t, agentd.PermProcessTemplatesManage, "")
	code, body = h.call("POST", "/v1/process/groups/crew/processes", map[string]any{"templateId": "gate"})
	require.Equal(t, 404, code, "crew is not visible: %s", body)
	h.grant(t, agentd.PermGroupsSettings, "group=crew")
	code, body = h.call("POST", "/v1/process/groups/crew/processes", map[string]any{"templateId": "gate"})
	require.Equal(t, 200, code, body)
	code, body = h.call("DELETE", "/v1/process/groups/crew/processes/gate", nil)
	require.Equal(t, 200, code, body)
	code, body = h.call("DELETE", "/v1/process/templates/prog", nil)
	require.Equal(t, 200, code, body)
}
