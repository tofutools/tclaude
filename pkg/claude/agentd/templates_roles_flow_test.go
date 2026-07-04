package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// JOH-240: the role library. A template roster agent REFERENCES a role and
// inherits its defaults — a canonical role-brief (rendered as a "## Role" block
// in the agent's startup context), a default launch shape, and a default
// permission set — BENEATH the agent's own overrides. These flow tests assert
// the resolved effects at real surfaces: the Spawner boundary (SpawnModel), the
// granted-permission rows, and the composed spawn-context inbox message.

// createRole POSTs a role to the library as the human.
func createRole(t *testing.T, f *testharness.Flow, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/roles", body))
	return testharness.Serve(f.Mux, r)
}

// Scenario: a template agent references a role that carries a default model, a
// default permission set, and a canonical brief. On instantiate the spawned
// agent (a) launches on the role's model, (b) is granted the role's permission,
// and (c) sees the role's brief as a "## Role" block in its startup context.
// This is the core win: the role's defaults reach the spawned agent.
func TestGroupTemplate_RoleRef_AppliesDefaults(t *testing.T) {
	f := newFlow(t)

	// A custom (non-seed) role so the create doesn't collide with the
	// self-healing seed library.
	require.Equalf(t, http.StatusCreated, createRole(t, f, map[string]any{
		"name":        "cold-reviewer",
		"descr":       "cold reviewer",
		"brief":       "You review changes with fresh eyes.",
		"model":       "haiku",
		"permissions": []string{"human.notify"},
	}).Code, "create role")

	createBody := map[string]any{
		"name": "review-team",
		"agents": []map[string]any{
			// References the role, brings nothing of its own — pure inheritance.
			{"name": "rev", "role_ref": "cold-reviewer"},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/review-team/instantiate",
		map[string]any{"group_name": "sentinel"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned, "the agent spawned")
	require.Equal(t, 0, res.Failed, "no spawn failures: %+v", res.Agents)
	agentd.WaitForBackgroundForTest()

	conv := res.Agents[0].ConvID
	require.NotEmpty(t, conv, "rev conv-id")

	// (a) Launched on the role's default model.
	model, ok := f.World.SpawnModel(conv)
	require.Truef(t, ok, "no spawn recorded for conv %s", conv)
	assert.Equal(t, "haiku", model, "agent inherits the role's default model")

	// (b) Granted the role's default permission.
	assert.Contains(t, res.Agents[0].Granted, "human.notify",
		"the role's default permission is granted at instantiate")
	perms, err := db.ListAgentPermissionsForConv(conv)
	require.NoError(t, err)
	assert.Containsf(t, perms, "human.notify",
		"the role's default permission is persisted on the conv: %+v", perms)

	// (c) The role's brief renders as a "## Role" block in the startup context.
	msg := soleInboxMessage(t, conv)
	assert.Contains(t, msg.Body, "## Role", "composed context carries the ## Role block")
	assert.Contains(t, msg.Body, "You review changes with fresh eyes.",
		"the role's canonical brief is in the composed context")
}

// Scenario: the agent's own fields WIN over the role's (role = defaults, agent =
// specifics). The agent pins an inline model and brings an extra permission; the
// resolved launch uses the agent's model and the grant set is the UNION.
func TestGroupTemplate_RoleRef_AgentOverridesWinAndPermsUnion(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, createRole(t, f, map[string]any{
		"name":        "coder",
		"model":       "haiku",
		"permissions": []string{"human.notify"},
	}).Code, "create role")

	createBody := map[string]any{
		"name": "team",
		"agents": []map[string]any{
			// Inline model overrides the role's; the agent adds its own perm.
			{"name": "d1", "role_ref": "coder", "model": "opus", "permissions": []string{"groups.spawn"}},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/instantiate",
		map[string]any{"group_name": "g1"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned)
	require.Equal(t, 0, res.Failed, "%+v", res.Agents)
	agentd.WaitForBackgroundForTest()

	conv := res.Agents[0].ConvID
	model, ok := f.World.SpawnModel(conv)
	require.True(t, ok)
	assert.Equal(t, "opus", model, "the agent's inline model wins over the role default")

	// The grant set is the union of role + agent permissions, deduped.
	assert.ElementsMatch(t, []string{"human.notify", "groups.spawn"}, res.Agents[0].Granted,
		"granted set is the role UNION agent permissions")
}

// Scenario: a role with no brief adds no "## Role" block — the block only
// appears when the referenced role carries guidance.
func TestGroupTemplate_RoleRef_NoBriefNoBlock(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, createRole(t, f, map[string]any{
		"name": "silent", "model": "haiku",
	}).Code, "create role")
	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name":   "t",
		"agents": []map[string]any{{"name": "a", "role_ref": "silent"}},
	}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/t/instantiate",
		map[string]any{"group_name": "g", "task": "Ship it."})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned)
	agentd.WaitForBackgroundForTest()

	msg := soleInboxMessage(t, res.Agents[0].ConvID)
	assert.NotContains(t, msg.Body, "## Role", "no brief ⇒ no ## Role block")
	assert.Contains(t, msg.Body, "Ship it.", "the task context still lands")
}

// Scenario: a template referencing a non-existent role is a 400 at save — the
// existence check is at the wire boundary (no DB-level FK), nothing is stored.
func TestGroupTemplate_RoleRef_RejectsUnknownRole(t *testing.T) {
	f := newFlow(t)
	rec := humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name":   "t",
		"agents": []map[string]any{{"name": "a", "role_ref": "ghost"}},
	})
	assert.Equalf(t, http.StatusBadRequest, rec.Code,
		"unknown role reference should 400; body=%s", rec.Body.String())
	tmpl, err := db.GetGroupTemplate("t")
	require.NoError(t, err)
	assert.Nil(t, tmpl, "rejected template must not be stored")
}

// Scenario: the reserved routing name "all" cannot be a role name.
func TestRole_RejectsReservedName(t *testing.T) {
	f := newFlow(t)
	rec := createRole(t, f, map[string]any{"name": "all"})
	assert.Equalf(t, http.StatusBadRequest, rec.Code,
		"reserved role name should 400; body=%s", rec.Body.String())
}

// Scenario: the canonical seed roles are present via the read API right after a
// fresh daemon boot (self-healing seed on Open).
func TestRoles_SeededOnBoot(t *testing.T) {
	f := newFlow(t)
	rec := humanReq(t, f, http.MethodGet, "/v1/roles", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "list roles: %s", rec.Body.String())
	var roles []struct {
		Name  string `json:"name"`
		Brief string `json:"brief"`
	}
	testharness.DecodeJSON(t, rec, &roles)
	byName := map[string]string{}
	for _, r := range roles {
		byName[r.Name] = r.Brief
	}
	for _, want := range []string{"po", "lead", "dev", "designer", "reviewer", "tester"} {
		brief, ok := byName[want]
		assert.Truef(t, ok, "seed role %q present", want)
		assert.NotEmptyf(t, brief, "seed role %q has a brief", want)
	}
}

// Scenario: export embeds the referenced role, and import onto a machine that
// lacks it re-creates the role — the export stays portable (JOH-240 / #763).
func TestTemplate_ExportImport_EmbedsAndRecreatesRole(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, createRole(t, f, map[string]any{
		"name": "auditor", "brief": "You audit.", "model": "haiku",
	}).Code, "create role")
	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name":   "audit-team",
		"agents": []map[string]any{{"name": "a", "role_ref": "auditor"}},
	}).Code, "create template")

	// Export carries the embedded role definition.
	exp := humanReq(t, f, http.MethodGet, "/v1/templates/audit-team/export", nil)
	require.Equalf(t, http.StatusOK, exp.Code, "export: %s", exp.Body.String())
	var env struct {
		Template map[string]any   `json:"template"`
		Roles    []map[string]any `json:"roles"`
	}
	testharness.DecodeJSON(t, exp, &env)
	require.Len(t, env.Roles, 1, "export embeds the referenced role")
	assert.Equal(t, "auditor", env.Roles[0]["name"])

	// Simulate importing onto a machine that lacks the role: delete it, then
	// re-import under a new name. The role must be re-created.
	_, err := db.DeleteRole("auditor")
	require.NoError(t, err)

	envelope := map[string]any{
		"format":         "tclaude-task-force",
		"format_version": 1,
		"template":       env.Template,
		"roles":          env.Roles,
	}
	imp := humanReq(t, f, http.MethodPost, "/v1/templates/import?as=audit-team-2", envelope)
	require.Equalf(t, http.StatusCreated, imp.Code, "import: %s", imp.Body.String())

	revived, err := db.GetRole("auditor")
	require.NoError(t, err)
	require.NotNil(t, revived, "import re-created the missing role")
	assert.Equal(t, "You audit.", revived.Brief, "re-created role keeps its brief")
}
