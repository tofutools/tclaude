package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
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
	Wave           int      `json:"wave,omitempty"`
}

// instantiateResult mirrors the JSON the instantiate endpoint returns.
type instantiateResult struct {
	Group   string `json:"group"`
	Spawned int    `json:"spawned"`
	Failed  int    `json:"failed"`
	Agents  []struct {
		Name           string   `json:"name"`
		FinalName      string   `json:"final_name"`
		ConvID         string   `json:"conv_id"`
		Owner          bool     `json:"owner"`
		WorktreePath   string   `json:"worktree_path"`
		WorktreeBranch string   `json:"worktree_branch"`
		Granted        []string `json:"granted"`
		Error          string   `json:"error"`
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
}

func TestGroupTemplate_InstantiatePerAgentWorktrees(t *testing.T) {
	f := newFlow(t)
	repo, parent := initRepoOnMain(t)

	createBody := map[string]any{
		"name": "wt-team",
		"agents": []templateAgentSpec{
			{Name: "lead"},
			{Name: "dev"},
		},
	}
	require.Equal(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code,
		"create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/wt-team/instantiate",
		map[string]any{
			"group_name": "wtforce",
			"cwd":        repo,
			"per_agent_worktrees": map[string]any{
				"repo":            repo,
				"branch_prefix":   "wtforce",
				"from_branch":     "main",
				"worktree_as_cwd": true,
			},
		})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())

	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 2, res.Spawned, "all agents spawned")
	require.Equal(t, 0, res.Failed, "no worktree/spawn failures")

	want := map[string]string{
		"lead": filepath.Join(parent, "repo-wtforce-lead"),
		"dev":  filepath.Join(parent, "repo-wtforce-dev"),
	}
	for _, a := range res.Agents {
		wantPath := want[a.Name]
		require.NotEmpty(t, wantPath, "unexpected agent %s", a.Name)
		assert.Equal(t, resolveSym(t, wantPath), resolveSym(t, a.WorktreePath), "response worktree path")
		assert.Equal(t, "wtforce-"+a.Name, a.WorktreeBranch, "response worktree branch")

		rows, err := db.FindSessionsByConvID(a.ConvID)
		require.NoError(t, err, "FindSessionsByConvID(%s)", a.ConvID)
		require.NotEmpty(t, rows, "session row for %s", a.Name)
		assert.Equal(t, resolveSym(t, wantPath), resolveSym(t, rows[0].Cwd),
			"agent %s launched in its own worktree", a.Name)
	}

	wts, err := worktree.ListWorktreesIn(repo)
	require.NoError(t, err, "ListWorktreesIn")
	branches := map[string]bool{}
	for _, wt := range wts {
		branches[wt.Branch] = true
	}
	assert.True(t, branches["wtforce-lead"], "lead worktree branch listed")
	assert.True(t, branches["wtforce-dev"], "dev worktree branch listed")
}

// Scenario (JOH-356): the "Form a party" dialog lets the human edit the
// template's shared context into the group's OWN copy before submitting. The
// instantiate endpoint accepts a context_override that REPLACES the template's
// default_context as the base the group context is composed from — the task is
// still folded in under ## Task, and the stored template is left untouched.
//
// Real-surface assertions: the created group's DefaultContext (what every
// member's briefing is composed from) reflects the edited copy, NOT the
// template boilerplate; the task is still present; and re-reading the template
// shows its stored context unchanged.
func TestGroupTemplate_InstantiateContextOverride(t *testing.T) {
	f := newFlow(t)

	const boilerplate = "STORED-TEMPLATE-CONTEXT: the reusable circle lore."
	createBody := map[string]any{
		"name":            "party-circle",
		"descr":           "a lead and a hand",
		"default_context": boilerplate,
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", InitialMessage: "Lead the party.", IsOwner: true},
			{Name: "hand", Role: "hand", InitialMessage: "Assist the lead."},
		},
	}
	require.Equal(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code,
		"create template")

	// The human edited the prefilled context into the group's own copy and
	// typed a task — exactly what the Form-a-party picker submits.
	const edited = "EDITED-GROUP-CONTEXT: this party's own tweaked brief."
	const task = "Chart the northern caves."
	rec := humanReq(t, f, http.MethodPost, "/v1/templates/party-circle/instantiate",
		map[string]any{
			"group_name":       "expedition",
			"task":             task,
			"context_override": edited,
		})
	require.Equal(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())

	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	assert.Equal(t, 2, res.Spawned, "both agents spawned")
	assert.Equal(t, 0, res.Failed, "no spawn failures")

	agentd.WaitForBackgroundForTest()

	// The group's composed context carries the EDITED copy + the task, and NOT
	// the stored template boilerplate the override replaced.
	g, err := db.GetAgentGroupByName("expedition")
	require.NoError(t, err)
	require.NotNil(t, g, "group expedition created")
	assert.Contains(t, g.DefaultContext, edited, "group context uses the edited override")
	assert.Contains(t, g.DefaultContext, task, "group context still carries the task")
	assert.NotContains(t, g.DefaultContext, boilerplate,
		"the override replaced the template boilerplate, not appended to it")

	// The stored template is untouched — the group got its OWN copy.
	tmpl, err := db.GetGroupTemplate("party-circle")
	require.NoError(t, err)
	require.NotNil(t, tmpl)
	assert.Equal(t, boilerplate, tmpl.DefaultContext,
		"the stored template context is unchanged by the override")

	// A member's briefing is composed from the edited copy, not the boilerplate.
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	require.NotEmpty(t, members)
	msgs, err := db.ListAgentMessagesForConv(members[0].ConvID, 100)
	require.NoError(t, err)
	require.NotEmpty(t, msgs, "member has an inbox briefing")
	assert.Contains(t, msgs[0].Body, edited, "member briefing carries the edited context")
	assert.NotContains(t, msgs[0].Body, boilerplate, "member briefing drops the replaced boilerplate")
}

// Scenario: instantiating a template can create the new group as a subgroup of
// an existing group while still using the caller's mirrored context override.
func TestGroupTemplate_InstantiateParentNestsNewGroup(t *testing.T) {
	f := newFlow(t)

	parentID, err := db.CreateAgentGroup("parent", "parent settings source")
	require.NoError(t, err)
	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{"name": "crew", "agents": []templateAgentSpec{{Name: "lead"}}}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/crew/instantiate",
		map[string]any{
			"group_name":       "child-force",
			"parent":           "parent",
			"context_override": "mirrored parent context",
			"descr_override":   "",
		})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate as subgroup: %s", rec.Body.String())
	agentd.WaitForBackgroundForTest()

	g, err := db.GetAgentGroupByName("child-force")
	require.NoError(t, err)
	require.NotNil(t, g)
	require.NotNil(t, g.ParentGroupID, "new group is nested")
	assert.Equal(t, parentID, *g.ParentGroupID, "parent pointer records the selected group")
	assert.Equal(t, "mirrored parent context", g.DefaultContext, "context override is the new group's own mirrored copy")
	assert.Empty(t, g.Descr, "empty descr_override stays empty")
}

// Scenario (JOH-385): the summon dialog's copy mode ("new group in this group's
// image") copies the drop target's description into the new group by POSTing a
// descr_override to instantiate. descr_override mirrors context_override's
// grammar — present = honored verbatim INCLUDING empty, absent = today's
// default — so a source group whose description is EMPTY can be faithfully
// copied (an explicit "" suppresses the "Instantiated from template X" default
// that a bare/absent descr would trigger).
//
// This pins the four wire behaviors the copy-mode fix depends on, plus the
// both-fields precedence, all read off the real surface (the created group
// row's Descr):
//
//	(a) descr_override "" of an empty-descr source → an empty-descr group
//	(b) a non-empty descr_override → landed verbatim
//	(c) no descr + no override → today's "Instantiated from template X" default
//	(d) a plain descr alone → unchanged behavior
//	(e) BOTH descr and descr_override → descr_override wins (the more explicit)
func TestGroupTemplate_InstantiateDescrOverride(t *testing.T) {
	f := newFlow(t)

	require.Equal(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
			"name":   "descr-circle",
			"agents": []templateAgentSpec{{Name: "lead", Role: "lead", IsOwner: true}},
		}).Code, "create template")

	descrOf := func(group string) string {
		t.Helper()
		g, err := db.GetAgentGroupByName(group)
		require.NoError(t, err)
		require.NotNil(t, g, "group %s created", group)
		return g.Descr
	}

	// (a) copy of an empty-descr source: descr_override "" suppresses the default.
	require.Equal(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates/descr-circle/instantiate",
			map[string]any{"group_name": "copy-empty", "descr_override": ""}).Code,
		"instantiate (a)")
	assert.Empty(t, descrOf("copy-empty"),
		"descr_override \"\" yields a group with NO description (suppresses the default)")

	// (b) a non-empty descr_override lands verbatim.
	const verbatim = "backend squad — auth work"
	require.Equal(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates/descr-circle/instantiate",
			map[string]any{"group_name": "copy-label", "descr_override": verbatim}).Code,
		"instantiate (b)")
	assert.Equal(t, verbatim, descrOf("copy-label"),
		"a non-empty descr_override is honored verbatim")

	// (c) no descr + no override: today's default still applies.
	require.Equal(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates/descr-circle/instantiate",
			map[string]any{"group_name": "plain-default"}).Code,
		"instantiate (c)")
	assert.Equal(t, "Instantiated from template descr-circle", descrOf("plain-default"),
		"absent descr_override keeps the default-descr behavior")

	// (d) a plain descr alone (no override): unchanged behavior — the field wins
	// over the default, exactly as before this feature.
	require.Equal(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates/descr-circle/instantiate",
			map[string]any{"group_name": "plain-descr", "descr": "hand-typed descr"}).Code,
		"instantiate (d)")
	assert.Equal(t, "hand-typed descr", descrOf("plain-descr"),
		"a plain descr alone is unchanged by the new grammar")

	// (e) BOTH sent: descr_override wins over the plain descr (the more explicit
	// grammar — same precedence as context_override vs the template context).
	require.Equal(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates/descr-circle/instantiate",
			map[string]any{
				"group_name":     "both-fields",
				"descr":          "the plain descr (loses)",
				"descr_override": "the override (wins)",
			}).Code, "instantiate (e)")
	assert.Equal(t, "the override (wins)", descrOf("both-fields"),
		"when both are sent, descr_override wins over the plain descr")

	// (f) a whitespace-only descr_override trims to empty and still suppresses
	// the default — pins the backend trim (a source group can only ever hold a
	// trimmed descr, but a direct API caller could send whitespace).
	require.Equal(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates/descr-circle/instantiate",
			map[string]any{"group_name": "copy-blank", "descr_override": "   "}).Code,
		"instantiate (f)")
	assert.Empty(t, descrOf("copy-blank"),
		"a whitespace-only descr_override trims to empty (default still suppressed)")
}

// Scenario: a human snapshots a live group into a template. The
// template must carry the group's context plus one agent per member,
// with the owner flag set on the member that owns the group.
func TestGroupTemplate_FromGroupSnapshot(t *testing.T) {
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

	// JOH-344 #4: the response carries a blank_briefs count so the CLI/dashboard
	// can warn honestly. A from-group snapshot recovers no per-agent briefs, so
	// every snapshotted agent is blank.
	var res struct {
		BlankBriefs int `json:"blank_briefs"`
	}
	testharness.DecodeJSON(t, rec, &res)
	assert.Equal(t, 2, res.BlankBriefs, "both snapshotted agents come through with blank briefs")

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
}

// Scenario: the CRUD endpoints round-trip — create, fetch, replace
// (including a rename), then delete. After delete the template is gone.
func TestGroupTemplate_CRUDRoundTrip(t *testing.T) {
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
}

// Scenario (JOH-337): re-snapshotting an evolved group INTO its own
// template. A template is instantiated as a group; the group then gains
// a member; from-group without update still 409s, and with update it
// re-snapshots the template in place — recovering the original
// template-agent names from the "<group>-<name>" member titles, keeping
// their curated task briefs, and reporting the roster diff.
func TestGroupTemplate_FromGroupUpdateResnapshot(t *testing.T) {
	f := newFlow(t)

	createBody := map[string]any{
		"name":            "crew",
		"descr":           "curated descr",
		"default_context": "CREW-CONTEXT",
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", InitialMessage: "Lead the crew.", IsOwner: true},
			{Name: "dev1", Role: "dev", InitialMessage: "Build features."},
		},
		"work_pattern": []map[string]string{
			{"send_to": "lead", "value": "Lead the charge: {{task}}"},
		},
	}
	require.Equal(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code,
		"create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/crew/instantiate",
		map[string]any{"group_name": "voyage"})
	require.Equal(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	// Instantiate queues async brief/pattern deliveries; drain them so the
	// spawn below doesn't race the flush worker's DB writes (SQLITE_BUSY).
	agentd.WaitForBackgroundForTest()

	// The group evolves: an extra member joins after instantiation.
	f.AsHuman().Spawn("voyage", "navigator")
	agentd.WaitForBackgroundForTest()

	// Without update, the taken name stays a hard conflict.
	rec = humanReq(t, f, http.MethodPost, "/v1/templates/from-group",
		map[string]any{"group": "voyage", "template_name": "crew"})
	require.Equal(t, http.StatusConflict, rec.Code, "plain from-group must 409: %s", rec.Body.String())

	// With update, the template is re-snapshotted in place.
	rec = humanReq(t, f, http.MethodPost, "/v1/templates/from-group",
		map[string]any{"group": "voyage", "template_name": "crew", "update": true})
	require.Equal(t, http.StatusOK, rec.Code, "update from-group: %s", rec.Body.String())

	var res struct {
		Name        string   `json:"name"`
		Updated     bool     `json:"updated"`
		BriefsKept  []string `json:"briefs_kept"`
		Added       []string `json:"added"`
		Removed     []string `json:"removed"`
		BlankBriefs int      `json:"blank_briefs"`
	}
	testharness.DecodeJSON(t, rec, &res)
	assert.Equal(t, "crew", res.Name)
	assert.True(t, res.Updated, "response reports an in-place update")
	assert.ElementsMatch(t, []string{"lead", "dev1"}, res.BriefsKept,
		"curated briefs survive for round-tripped agents")
	require.Len(t, res.Added, 1, "the post-instantiate joiner is reported as added")
	assert.Empty(t, res.Removed, "nobody left the group")
	// JOH-344 #4: lead + dev1 kept their briefs; only the joiner is blank.
	assert.Equal(t, 1, res.BlankBriefs, "only the newly-added joiner has a blank brief")

	// Real surface: the stored template after the update.
	tmpl, err := db.GetGroupTemplate("crew")
	require.NoError(t, err)
	require.NotNil(t, tmpl)
	assert.Equal(t, "CREW-CONTEXT", tmpl.DefaultContext,
		"context re-traced from the group (which inherited it at instantiate)")
	require.Len(t, tmpl.Agents, 3, "two round-tripped agents + the joiner")

	byName := map[string]db.GroupTemplateAgent{}
	for _, a := range tmpl.Agents {
		byName[a.Name] = a
	}
	require.Contains(t, byName, "lead", "lead's template name round-trips")
	require.Contains(t, byName, "dev1", "dev1's template name round-trips")
	assert.Equal(t, "Lead the crew.", byName["lead"].InitialMessage, "lead's curated brief survives")
	assert.Equal(t, "Build features.", byName["dev1"].InitialMessage, "dev1's curated brief survives")
	assert.True(t, byName["lead"].IsOwner, "owner flag re-traced from the live group")
	assert.Equal(t, "", byName[res.Added[0]].InitialMessage, "the joiner comes in with a blank brief")

	// The curated template descr survives — instantiate stamps the group
	// with "Instantiated from template crew", which must NOT leak back
	// into the blueprint's own description on a re-snapshot.
	assert.Equal(t, "curated descr", tmpl.Descr, "instance descr never clobbers curated template descr")

	// The work pattern is blueprint choreography — a live group has none
	// to trace, so the update re-snapshot must keep the curated one.
	require.Len(t, tmpl.WorkPattern, 1, "work pattern survives the update re-snapshot")
	assert.Equal(t, "lead", tmpl.WorkPattern[0].SendTo)
	assert.Equal(t, "Lead the charge: {{task}}", tmpl.WorkPattern[0].Value)

	// Round two: dev1's member leaves the group, then another update
	// re-snapshot. The departed agent is reported removed, lead's brief
	// still survives, and the joiner — whose conv title is exactly
	// "navigator" with no "voyage-" prefix (it was named at spawn, not
	// by instantiate) — round-trips through the EXACT-title recover
	// candidate, keeping its template name stable even though its
	// roster position shifted.
	joinerName := res.Added[0]
	var membersList []struct {
		ConvID string `json:"conv_id"`
		Title  string `json:"title"`
	}
	rec = humanReq(t, f, http.MethodGet, "/v1/groups/voyage/members", nil)
	require.Equal(t, http.StatusOK, rec.Code, "list members: %s", rec.Body.String())
	testharness.DecodeJSON(t, rec, &membersList)
	dev1Conv := ""
	for _, m := range membersList {
		if m.Title == "voyage-dev1" {
			dev1Conv = m.ConvID
		}
	}
	require.NotEmpty(t, dev1Conv, "voyage-dev1 member resolvable by title")
	rec = humanReq(t, f, http.MethodDelete, "/v1/groups/voyage/members/"+dev1Conv, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "remove dev1's member: %s", rec.Body.String())

	rec = humanReq(t, f, http.MethodPost, "/v1/templates/from-group",
		map[string]any{"group": "voyage", "template_name": "crew", "update": true})
	require.Equal(t, http.StatusOK, rec.Code, "second update from-group: %s", rec.Body.String())
	testharness.DecodeJSON(t, rec, &res)
	assert.ElementsMatch(t, []string{"lead"}, res.BriefsKept, "lead's brief survives round two")
	assert.Empty(t, res.Added, "the joiner round-trips by exact title, not as a new agent")
	assert.Equal(t, []string{"dev1"}, res.Removed, "the departed member is reported removed")

	tmpl, err = db.GetGroupTemplate("crew")
	require.NoError(t, err)
	require.NotNil(t, tmpl)
	names := []string{}
	for _, a := range tmpl.Agents {
		names = append(names, a.Name)
	}
	assert.ElementsMatch(t, []string{"lead", joinerName}, names,
		"dev1 dropped from the blueprint; the joiner kept its exact-title name")
	for _, a := range tmpl.Agents {
		if a.Name == "lead" {
			assert.Equal(t, "Lead the crew.", a.InitialMessage, "lead's curated brief survives round two")
		}
	}
	require.Len(t, tmpl.WorkPattern, 1, "work pattern survives repeated re-snapshots")
	assert.Equal(t, "lead", tmpl.WorkPattern[0].SendTo)
}

// Scenario (JOH-337): update:true against a name with NO existing
// template simply creates it — the flag is create-or-update, so a CLI
// `--update` habit or a dashboard race never errors on first use.
func TestGroupTemplate_FromGroupUpdateCreatesWhenMissing(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("solo")
	f.AsHuman().Spawn("solo", "worker")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/from-group",
		map[string]any{"group": "solo", "template_name": "solo-tmpl", "update": true})
	require.Equal(t, http.StatusCreated, rec.Code, "update-on-missing creates: %s", rec.Body.String())

	tmpl, err := db.GetGroupTemplate("solo-tmpl")
	require.NoError(t, err)
	require.NotNil(t, tmpl, "template created")
	assert.Len(t, tmpl.Agents, 1)
}

// Scenario (JOH-344 #3): deploying against a bare Linear-issue URL with no
// --group names the force after the issue KEY, not the template — so three
// deploys of one template against three issues are distinguishable, instead of
// dev-squad / dev-squad-2 / dev-squad-3.
func TestGroupTemplate_DeployBareIssueURL_NamesGroupFromKey(t *testing.T) {
	f := newFlow(t)

	createBody := map[string]any{
		"name": "dev-squad",
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", IsOwner: true},
		},
	}
	require.Equal(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/dev-squad/deploy",
		map[string]any{"mission": "https://linear.app/acme/issue/JOH-245/some-title"})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	agentd.WaitForBackgroundForTest()

	var res struct {
		Group string `json:"group"`
	}
	testharness.DecodeJSON(t, rec, &res)
	assert.Equal(t, "joh-245", res.Group, "group named after the issue key, not the template")

	g, err := db.GetAgentGroupByName("joh-245")
	require.NoError(t, err)
	assert.NotNil(t, g, "the force's group is discoverable under the issue-key name")
}

// Scenario (JOH-336): a template with a work pattern — an ordered list
// of routed briefing messages — delivers it after the whole roster has
// spawned: step 1 to the lead only (with {{task}} interpolated), step 2
// to every member. Assertions run at the real inbox surface
// (db.ListAgentMessagesForConv), the same rows `inbox read` renders.
func TestGroupTemplate_WorkPatternDelivery(t *testing.T) {
	f := newFlow(t)

	createBody := map[string]any{
		"name": "led-crew",
		"agents": []templateAgentSpec{
			{Name: "lead", Role: "lead", InitialMessage: "You lead.", IsOwner: true},
			{Name: "dev1", Role: "dev"},
		},
		"work_pattern": []map[string]string{
			{"send_to": "lead", "value": "You run this force. Distribute: {{task}}"},
			{"send_to": "all", "value": "House rules: open PRs, report at milestones."},
		},
	}
	rec := humanReq(t, f, http.MethodPost, "/v1/templates", createBody)
	require.Equal(t, http.StatusCreated, rec.Code, "create template: %s", rec.Body.String())

	const task = "Ship the OAuth login epic."
	rec = humanReq(t, f, http.MethodPost, "/v1/templates/led-crew/instantiate",
		map[string]any{"group_name": "sortie", "task": task})
	require.Equal(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())

	var res struct {
		Agents []struct {
			Name   string `json:"name"`
			ConvID string `json:"conv_id"`
		} `json:"agents"`
		PatternDelivered int      `json:"pattern_delivered"`
		PatternErrors    []string `json:"pattern_errors"`
	}
	testharness.DecodeJSON(t, rec, &res)
	// 1 (lead) + 2 (all) deliveries, no errors.
	assert.Equal(t, 3, res.PatternDelivered, "lead step + all-step×2 delivered")
	assert.Empty(t, res.PatternErrors)

	convs := map[string]string{}
	for _, a := range res.Agents {
		convs[a.Name] = a.ConvID
	}
	require.Contains(t, convs, "lead")
	require.Contains(t, convs, "dev1")

	bodiesFor := func(conv string) []string {
		msgs, err := db.ListAgentMessagesForConv(conv, 100)
		require.NoError(t, err)
		out := []string{}
		for _, m := range msgs {
			out = append(out, m.Body)
		}
		return out
	}
	joined := func(conv string) string { return strings.Join(bodiesFor(conv), "\n---\n") }

	// The lead got the leader step WITH the task interpolated, plus the
	// all-members step.
	leadInbox := joined(convs["lead"])
	assert.Contains(t, leadInbox, "You run this force. Distribute: "+task,
		"lead briefing carries the interpolated task")
	assert.Contains(t, leadInbox, "House rules", "lead also gets the all-members step")

	// dev1 got the all-members step but NOT the leader step.
	devInbox := joined(convs["dev1"])
	assert.Contains(t, devInbox, "House rules", "dev1 gets the all-members step")
	assert.NotContains(t, devInbox, "You run this force",
		"the leader-routed step goes to the lead only")

	// The stored template round-trips the pattern through the wire shape.
	tmpl, err := db.GetGroupTemplate("led-crew")
	require.NoError(t, err)
	require.NotNil(t, tmpl)
	require.Len(t, tmpl.WorkPattern, 2)
	assert.Equal(t, "lead", tmpl.WorkPattern[0].SendTo)
	assert.Equal(t, "all", tmpl.WorkPattern[1].SendTo)
}

// Scenario (JOH-336): work-pattern validation — a step routed to a
// non-roster name is rejected at template save with a clear error.
func TestGroupTemplate_WorkPatternValidation(t *testing.T) {
	f := newFlow(t)

	rec := humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name":   "bad-pattern",
		"agents": []templateAgentSpec{{Name: "dev1"}},
		"work_pattern": []map[string]string{
			{"send_to": "nobody", "value": "hello"},
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "unknown send_to must 400: %s", rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, "nobody", "the error names the bad target")
	assert.Contains(t, body, "dev1", "the error lists the valid roster targets")
	assert.Contains(t, body, "broadcast", "the error explains the \"all\" broadcast target")
}
