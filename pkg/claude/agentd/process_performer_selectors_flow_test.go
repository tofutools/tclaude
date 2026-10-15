package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-52p3cq: an agent step's "who" is a specific agent, any member with a
// role, or anyone in the run's scope; and what happens when nobody can take
// it — spawn, wait, or fail — is set per step or once for the process,
// separately from the role.

func selectorPerformer(edit func(*model.Performer)) model.Performer {
	performer := model.Performer{Kind: model.PerformerAgent, Prompt: "Review the change."}
	edit(&performer)
	return performer
}

func groupScope(g *db.AgentGroup) agentd.ProcessRunScopeForTest {
	return agentd.ProcessRunScopeForTest{Kind: "group", GroupID: g.ID}
}

// Scenario: role picks a member by role alone. The step's legacy profile no
// longer decides who matches once a role is named.
func TestProcessPerformerSelector_RolePicksAMemberWithThatRole(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	havePerformerAgent(t, f, "crew", "devv-1111-2222-3333-4444", "alice", "dev", "idle")
	carol := havePerformerAgent(t, f, "crew", "revw-1111-2222-3333-4444", "carol", "reviewer", "idle")
	performer := selectorPerformer(func(p *model.Performer) { p.Role, p.Profile = "reviewer", "dev" })
	view := startPerformerRun(t, f, root, "run_role", performerTemplate("role", performer, nil), groupScope(g))
	require.NotNil(t, view.Commands[0].Performer)
	assert.Equal(t, carol, view.Commands[0].Performer.AgentID)
	assert.Equal(t, "role", view.Commands[0].Performer.Resolution)
}

// Scenario: anyone takes any member: idle before busy, and among idle members
// the one holding fewer open process attempts.
func TestProcessPerformerSelector_AnyonePrefersIdleThenLeastBusy(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	havePerformerAgent(t, f, "crew", "busy-1111-2222-3333-4444", "bob", "dev", "working")
	carol := havePerformerAgent(t, f, "crew", "revw-1111-2222-3333-4444", "carol", "reviewer", "idle")
	dave := havePerformerAgent(t, f, "crew", "dave-1111-2222-3333-4444", "dave", "", "idle")
	performer := selectorPerformer(func(p *model.Performer) { p.Anyone = true })

	first := startPerformerRun(t, f, root, "run_anyone_1", performerTemplate("anyone", performer, nil), groupScope(g))
	require.NotNil(t, first.Commands[0].Performer)
	firstAgent := first.Commands[0].Performer.AgentID
	assert.Contains(t, []string{carol, dave}, firstAgent, "an idle member is preferred over the working one")
	assert.Equal(t, "anyone", first.Commands[0].Performer.Resolution)

	second := startPerformerRun(t, f, root, "run_anyone_2", performerTemplate("anyone", performer, nil), groupScope(g))
	require.NotNil(t, second.Commands[0].Performer)
	assert.Contains(t, []string{carol, dave}, second.Commands[0].Performer.AgentID)
	assert.NotEqual(t, firstAgent, second.Commands[0].Performer.AgentID,
		"the idle member already holding an open attempt is the busier one")
}

// Scenario: a spawn fallback with no profile spawns the group's default spawn
// profile; with no default it waits and says why.
func TestProcessPerformerFallback_SpawnUsesTheGroupDefaultProfile(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")
	_, err := db.SetAgentGroupDefaultCwd("crew", t.TempDir())
	require.NoError(t, err)
	performer := selectorPerformer(func(p *model.Performer) {
		p.Role = "qa"
		p.Fallback = &model.PerformerFallback{Action: model.FallbackSpawn}
	})

	waiting := startPerformerRun(t, f, root, "run_spawn_nodefault", performerTemplate("spawn-default", performer, nil), groupScope(g))
	assert.Equal(t, db.ProcessDispatchNeedsPerformer, waiting.Commands[0].State)
	require.NotNil(t, waiting.Commands[0].Performer)
	assert.Contains(t, waiting.Commands[0].Performer.WaitReason, `no group member has role "qa"`)
	assert.Contains(t, waiting.Commands[0].Performer.WaitReason, "the group has none")

	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "sonnet-dev", Harness: "claude"})
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultProfile("crew", "sonnet-dev")
	require.NoError(t, err)
	view := startPerformerRun(t, f, root, "run_spawn_default", performerTemplate("spawn-default", performer, nil), groupScope(g))
	require.NotNil(t, view.Commands[0].Performer, "%+v", view)
	assert.Equal(t, db.ProcessDispatchDelivered, view.Commands[0].State, "%+v", view.Commands[0].Performer)
	assert.Equal(t, "spawned", view.Commands[0].Performer.Resolution)
	conv, err := db.CurrentConvForAgent(view.Commands[0].Performer.AgentID)
	require.NoError(t, err)
	member, err := db.FindMemberInGroup(g.ID, conv)
	require.NoError(t, err)
	require.NotNil(t, member, "the spawned performer joins the run's group")
}

// Scenario: the process-wide fallback applies to every agent step, a step's
// own fallback overrides it, and fail fails the attempt without a performer.
func TestProcessPerformerFallback_ProcessDefaultFailAndStepOverride(t *testing.T) {
	f, root := processRuntimeFlow(t)
	g := f.HaveGroup("crew")

	failing := performerTemplate("fallback-fail", selectorPerformer(func(p *model.Performer) { p.Role = "qa" }), nil)
	failing.Fallback = &model.PerformerFallback{Action: model.FallbackFail}
	view := startPerformerRun(t, f, root, "run_fallback_fail", failing, groupScope(g))
	assert.Equal(t, engine.RunFailed, view.Status, "%+v", view)
	require.Len(t, view.Performers, 1)
	assert.Equal(t, "fallback_failed", view.Performers[0].Resolution)
	assert.Equal(t, "failed", view.Performers[0].Outcome)
	assert.Empty(t, view.Performers[0].AgentID)
	assert.Contains(t, view.Performers[0].Note, `no group member has role "qa"; the step's fallback fails it`)

	waiting := performerTemplate("fallback-override", selectorPerformer(func(p *model.Performer) {
		p.Role = "qa"
		p.Fallback = &model.PerformerFallback{Action: model.FallbackWait}
	}), nil)
	waiting.Fallback = &model.PerformerFallback{Action: model.FallbackFail}
	held := startPerformerRun(t, f, root, "run_fallback_wait", waiting, groupScope(g))
	assert.Equal(t, engine.RunRunning, held.Status)
	assert.Equal(t, "needs_performer", held.Action)
	assert.Contains(t, held.Commands[0].Performer.WaitReason, "the step waits for one")
}

// Scenario: the template validates the new fields: one selector at most, a
// known fallback action, a profile only for a spawn, agent performers only.
func TestProcessPerformerSelector_Validation(t *testing.T) {
	f, _ := processRuntimeFlow(t)
	validate := func(performer model.Performer, tmplFallback *model.PerformerFallback) []string {
		t.Helper()
		tmpl := performerTemplate("selectors", performer, nil)
		tmpl.Fallback = tmplFallback
		rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/validate", map[string]any{"template": tmpl})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body struct {
			Diagnostics []struct {
				Code     string `json:"code"`
				Severity string `json:"severity"`
			} `json:"diagnostics"`
		}
		testharness.DecodeJSON(t, rec, &body)
		var codes []string
		for _, d := range body.Diagnostics {
			if d.Severity == "error" {
				codes = append(codes, d.Code)
			}
		}
		return codes
	}
	assert.Empty(t, validate(selectorPerformer(func(p *model.Performer) {
		p.Role = "reviewer"
		p.Fallback = &model.PerformerFallback{Action: model.FallbackSpawn, Profile: "sonnet-dev"}
	}), &model.PerformerFallback{Action: model.FallbackWait}))
	assert.Contains(t, validate(selectorPerformer(func(p *model.Performer) { p.Role, p.Anyone = "reviewer", true }), nil),
		"conflicting_performer_selector")
	assert.Contains(t, validate(selectorPerformer(func(p *model.Performer) {
		p.Fallback = &model.PerformerFallback{Action: "retry"}
	}), nil), "invalid_fallback")
	assert.Contains(t, validate(selectorPerformer(func(p *model.Performer) {
		p.Fallback = &model.PerformerFallback{Action: model.FallbackWait, Profile: "sonnet-dev"}
	}), nil), "invalid_fallback")
	assert.Contains(t, validate(selectorPerformer(func(p *model.Performer) {
		p.Spawn = model.SpawnAllowed
		p.Fallback = &model.PerformerFallback{Action: model.FallbackWait}
	}), nil), "conflicting_fallback")
	assert.Contains(t, validate(model.Performer{Kind: model.PerformerHuman, Ask: "Ship?", Role: "lead"}, nil), "kind_scoped_field")
	assert.Contains(t, validate(selectorPerformer(func(*model.Performer) {}), &model.PerformerFallback{Action: "later"}),
		"invalid_fallback")
}

// Scenario: the step list's "who it will use now" panel previews every agent
// step against a group without starting a run, and the process context gives
// each group its default spawn profile and member roles — including a group
// with no process runs yet.
func TestProcessResolvePreviewAndGroupContext(t *testing.T) {
	f, _ := processRuntimeFlow(t)
	f.HaveGroup("crew")
	alice := havePerformerAgent(t, f, "crew", "devv-1111-2222-3333-4444", "alice", "dev", "idle")
	havePerformerAgent(t, f, "crew", "devb-1111-2222-3333-4444", "bob", "dev", "working")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "sonnet-dev", Harness: "claude"})
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultProfile("crew", "sonnet-dev")
	require.NoError(t, err)

	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "preview", Start: "start",
		Fallback: &model.PerformerFallback{Action: model.FallbackSpawn},
		Nodes: map[string]model.Node{
			"start":     {Type: model.NodeTypeStart, Next: model.Next{"next": "implement"}},
			"implement": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Role: "dev", Prompt: "Implement."}, Next: model.Next{"next": "review"}},
			"review":    {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Role: "reviewer", Prompt: "Review."}, Next: model.Next{"next": "qa"}},
			"qa": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Role: "qa", Prompt: "Test.",
				Fallback: &model.PerformerFallback{Action: model.FallbackFail}}, Next: model.Next{"next": "approve"}},
			"approve": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Ship?"}, Next: model.Next{"next": "end"}},
			"end":     {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
	rec := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/resolve-preview", map[string]any{"template": tmpl, "group": "crew"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var preview struct {
		Nodes map[string]struct {
			AgentID      string `json:"agentId"`
			Name         string `json:"name"`
			Reason       string `json:"reason"`
			SpawnProfile string `json:"spawnProfile"`
			Waits        bool   `json:"waits"`
			Fails        bool   `json:"fails"`
		} `json:"nodes"`
	}
	testharness.DecodeJSON(t, rec, &preview)
	require.Len(t, preview.Nodes, 3, "only agent steps are previewed: %+v", preview.Nodes)
	assert.Equal(t, alice, preview.Nodes["implement"].AgentID, "the idle dev")
	assert.Equal(t, "alice", preview.Nodes["implement"].Name)
	assert.Contains(t, preview.Nodes["implement"].Reason, "idle now")
	assert.Equal(t, "sonnet-dev", preview.Nodes["review"].SpawnProfile, "the process fallback spawns the group default")
	assert.True(t, preview.Nodes["qa"].Fails, "the step's own fallback overrides the process default")

	rec = processRuntimeRequest(t, f, http.MethodPost, "/v1/process/resolve-preview", map[string]any{"template": tmpl, "group": "nope"})
	assert.Equal(t, http.StatusNotFound, rec.Code)

	rec = processRuntimeRequest(t, f, http.MethodGet, "/v1/process/context", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var context struct {
		Groups []struct {
			Group               string `json:"group"`
			DefaultSpawnProfile string `json:"defaultSpawnProfile"`
			Roles               []struct {
				Role  string `json:"role"`
				Count int    `json:"count"`
			} `json:"roles"`
		} `json:"groups"`
	}
	testharness.DecodeJSON(t, rec, &context)
	var found bool
	for _, g := range context.Groups {
		if g.Group != "crew" {
			continue
		}
		found = true
		assert.Equal(t, "sonnet-dev", g.DefaultSpawnProfile)
		require.Len(t, g.Roles, 1)
		assert.Equal(t, "dev", g.Roles[0].Role)
		assert.Equal(t, 2, g.Roles[0].Count)
	}
	assert.True(t, found, "a group with no runs is listed: %s", rec.Body.String())
}
