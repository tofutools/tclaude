package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// humanReq issues an HTTP request against the daemon mux as the human
// (cookie-authed-dashboard-equivalent) and returns the recorder.
func humanReq(t *testing.T, f *testharness.Flow, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, method, path, body))
	return testharness.Serve(f.Mux, r)
}

// templateAgentSpec is the wire shape this test posts for one agent in
// a template body — mirrors templateAgentJSON in templates.go.
type templateAgentSpec struct {
	Name           string   `json:"name"`
	Role           string   `json:"role,omitempty"`
	Descr          string   `json:"descr,omitempty"`
	InitialMessage string   `json:"initial_message,omitempty"`
	IsOwner        bool     `json:"is_owner,omitempty"`
	Permissions    []string `json:"permissions,omitempty"`
}

// instantiateResult mirrors the JSON the instantiate endpoint returns.
type instantiateResult struct {
	Group   string `json:"group"`
	Spawned int    `json:"spawned"`
	Failed  int    `json:"failed"`
	Agents  []struct {
		Name      string   `json:"name"`
		FinalName string   `json:"final_name"`
		ConvID    string   `json:"conv_id"`
		Owner     bool     `json:"owner"`
		Granted   []string `json:"granted"`
		Error     string   `json:"error"`
	} `json:"agents"`
}

// Scenario: a human defines a 3-agent template — a PO marked owner and
// granted groups.spawn, plus two devs — then instantiates a group from
// it. The daemon must create the group, spawn one agent per spec with
// "<group>-<name>" final names, grant the owner ownership + its
// permission slugs, and fold the per-instantiation task into the group
// context every agent's inbox briefing carries.
//
// Real-surface assertions: the group + member rows, the owner row, the
// per-conv permission grant, and the "Startup context" inbox message —
// the same surfaces the dashboard and CLI read.
func TestGroupTemplate_InstantiateSpawnsTeam(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		const boilerplate = "TEAM-BOILERPLATE: use worktrees and open PRs."
		createBody := map[string]any{
			"name":            "feature-team",
			"descr":           "a PO and two devs",
			"default_context": boilerplate,
			"agents": []templateAgentSpec{
				{Name: "PO", Role: "product-owner", Descr: "leads", InitialMessage: "Coordinate the team.", IsOwner: true, Permissions: []string{agentd.PermGroupsSpawn}},
				{Name: "dev1", Role: "dev", InitialMessage: "Build feature A."},
				{Name: "dev2", Role: "dev", InitialMessage: "Build feature B."},
			},
		}
		rec := humanReq(t, f, http.MethodPost, "/v1/templates", createBody)
		require.Equal(t, http.StatusCreated, rec.Code, "create template: %s", rec.Body.String())

		const task = "Build the OAuth2 login flow with PKCE."
		rec = humanReq(t, f, http.MethodPost, "/v1/templates/feature-team/instantiate",
			map[string]any{"group_name": "phoenix", "task": task})
		require.Equal(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())

		var res instantiateResult
		testharness.DecodeJSON(t, rec, &res)
		assert.Equal(t, "phoenix", res.Group)
		assert.Equal(t, 3, res.Spawned, "all three agents spawned")
		assert.Equal(t, 0, res.Failed, "no spawn failures")
		require.Len(t, res.Agents, 3)

		// Final names are "<group>-<template-agent-name>".
		poConv := ""
		for _, a := range res.Agents {
			assert.Equal(t, "phoenix-"+a.Name, a.FinalName, "final name for %s", a.Name)
			assert.Empty(t, a.Error, "agent %s spawned cleanly", a.Name)
			if a.Name == "PO" {
				poConv = a.ConvID
				assert.True(t, a.Owner, "PO is the group owner")
				assert.Contains(t, a.Granted, agentd.PermGroupsSpawn, "PO granted groups.spawn")
			}
		}
		require.NotEmpty(t, poConv, "PO conv-id in response")

		// The group exists with the task folded into its context.
		g, err := db.GetAgentGroupByName("phoenix")
		require.NoError(t, err)
		require.NotNil(t, g, "group phoenix created")
		assert.Contains(t, g.DefaultContext, boilerplate, "group context keeps template boilerplate")
		assert.Contains(t, g.DefaultContext, task, "group context carries the instantiation task")

		members, err := db.ListAgentGroupMembers(g.ID)
		require.NoError(t, err)
		assert.Len(t, members, 3, "three members joined")

		// Ownership landed on the PO's conv.
		owners, err := db.ListAgentGroupOwners(g.ID)
		require.NoError(t, err)
		require.Len(t, owners, 1, "exactly one owner")
		assert.Equal(t, poConv, owners[0].ConvID, "owner is the PO")

		// The PO's permission grant is a real per-conv override.
		perms, err := db.ListAgentPermissionsForConv(poConv)
		require.NoError(t, err)
		assert.Contains(t, perms, agentd.PermGroupsSpawn, "PO holds groups.spawn")

		// Every member's startup briefing carries the task.
		for _, m := range members {
			msgs, err := db.ListAgentMessagesForConv(m.ConvID, 100)
			require.NoError(t, err)
			require.NotEmpty(t, msgs, "member %s has an inbox message", m.ConvID)
			assert.Contains(t, msgs[0].Body, task, "member %s briefing carries the task", m.ConvID)
		}
	})
}

// Scenario: a human snapshots a live group into a template. The
// template must carry the group's context plus one agent per member,
// with the owner flag set on the member that owns the group.
func TestGroupTemplate_FromGroupSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("src")

		const ctx = "SRC-GROUP-CONTEXT"
		require.Equal(t, http.StatusOK,
			humanReq(t, f, http.MethodPatch, "/v1/groups/src",
				map[string]any{"default_context": ctx}).Code,
			"set group context")

		lead := f.AsHuman().Spawn("src", "lead")
		f.AsHuman().Spawn("src", "helper")

		// Make the lead the group owner so the snapshot has an owner.
		require.Equal(t, http.StatusOK,
			humanReq(t, f, http.MethodPost, "/v1/groups/src/owners",
				map[string]any{"conv": lead.ConvID}).Code,
			"grant lead ownership")

		rec := humanReq(t, f, http.MethodPost, "/v1/templates/from-group",
			map[string]any{"group": "src", "template_name": "src-tmpl"})
		require.Equal(t, http.StatusCreated, rec.Code, "from-group: %s", rec.Body.String())

		tmpl, err := db.GetGroupTemplate("src-tmpl")
		require.NoError(t, err)
		require.NotNil(t, tmpl, "template created")
		assert.Equal(t, ctx, tmpl.DefaultContext, "template inherits the group context")
		require.Len(t, tmpl.Agents, 2, "one template agent per member")

		owners := 0
		for _, a := range tmpl.Agents {
			if a.IsOwner {
				owners++
			}
		}
		assert.Equal(t, 1, owners, "exactly one snapshotted agent is owner")
	})
}

// Scenario: the CRUD endpoints round-trip — create, fetch, replace
// (including a rename), then delete. After delete the template is gone.
func TestGroupTemplate_CRUDRoundTrip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		require.Equal(t, http.StatusCreated,
			humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
				"name":   "draft",
				"agents": []templateAgentSpec{{Name: "solo"}},
			}).Code, "create")

		// PATCH is a full replace — rename it and swap the agent list.
		require.Equal(t, http.StatusOK,
			humanReq(t, f, http.MethodPatch, "/v1/templates/draft", map[string]any{
				"name":  "final",
				"descr": "renamed",
				"agents": []templateAgentSpec{
					{Name: "a"}, {Name: "b"},
				},
			}).Code, "patch (rename + replace agents)")

		// The old name is gone; the new one carries the new state.
		assert.Equal(t, http.StatusNotFound,
			humanReq(t, f, http.MethodGet, "/v1/templates/draft", nil).Code,
			"old name 404s after rename")
		got, err := db.GetGroupTemplate("final")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "renamed", got.Descr)
		assert.Len(t, got.Agents, 2, "agent list replaced")

		require.Equal(t, http.StatusNoContent,
			humanReq(t, f, http.MethodDelete, "/v1/templates/final", nil).Code, "delete")
		gone, err := db.GetGroupTemplate("final")
		require.NoError(t, err)
		assert.Nil(t, gone, "template deleted")

		// A duplicate-name create is a 409.
		require.Equal(t, http.StatusCreated,
			humanReq(t, f, http.MethodPost, "/v1/templates",
				map[string]any{"name": "dup"}).Code, "first create")
		assert.Equal(t, http.StatusConflict,
			humanReq(t, f, http.MethodPost, "/v1/templates",
				map[string]any{"name": "dup"}).Code, "duplicate create 409s")
	})
}
