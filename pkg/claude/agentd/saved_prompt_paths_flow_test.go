package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func convMessagesContain(t *testing.T, conv, text string) bool {
	t.Helper()
	msgs, err := db.ListAgentMessagesForConv(conv, 50)
	require.NoError(t, err)
	for _, m := range msgs {
		if strings.Contains(m.Body, text) {
			return true
		}
	}
	return false
}

// A template agent's brief opens with its referenced profile's prompts, and
// one without a profile takes the group's default prompts.
func TestTemplateDeploy_ComposesSavedPrompts(t *testing.T) {
	f := newFlow(t)
	haveSavedPrompts(t, f, map[string]string{"kit-rules": "Follow the kit rules."})
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{"name": "kit", "prompt_refs": []string{"kit-rules"}}).Code)
	rec := humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "kit-team",
		"agents": []map[string]any{
			{"name": "worker", "spawn_profile": "kit", "initial_message": "Build feature A."},
			{"name": "plain", "initial_message": "Build feature B."},
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = humanReq(t, f, http.MethodPost, "/v1/templates/kit-team/instantiate", map[string]any{"group_name": "crew"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 0, res.Failed, rec.Body.String())
	convs := map[string]string{}
	for _, a := range res.Agents {
		convs[a.Name] = a.ConvID
	}
	assert.True(t, convMessagesContain(t, convs["worker"], "Follow the kit rules."))
	assert.True(t, convMessagesContain(t, convs["worker"], "Build feature A."))
	assert.False(t, convMessagesContain(t, convs["plain"], "Follow the kit rules."))
}

// Cron and trigger workers compose saved prompts: the profile's set, else the
// group's default prompts.
func TestManagedSpawn_ComposesSavedPrompts(t *testing.T) {
	f, job := cronSpawnFixture(t, "", 1)
	haveSavedPrompts(t, f, map[string]string{"group-rules": "Follow the group rules."})
	require.Equal(t, http.StatusOK, humanReq(t, f, http.MethodPatch, "/v1/groups/alpha", map[string]any{"default_prompts": []string{"group-rules"}}).Code)
	status, detail := runCronNowWithDetail(t, f, job.ID)
	require.Equal(t, "spawned", status, detail)
	runs, err := db.ListAgentCronRunsForJob(job.ID, 1)
	require.NoError(t, err)
	conv, err := db.CurrentConvForAgent(runs[0].WorkerAgent)
	require.NoError(t, err)
	assert.True(t, convMessagesContain(t, conv, "Follow the group rules."))
	assert.True(t, convMessagesContain(t, conv, "Check Linear for flaky-test tickets"))

	haveSavedPrompts(t, f, map[string]string{"review-rules": "Follow the review rules."})
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude", PromptRefs: []string{"review-rules"}})
	require.NoError(t, err)
	const author = "prompt-author-conv"
	f.HaveConvWithTitle(author, "author")
	f.HaveMember("alpha", author)
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	ruleID, err := db.InsertTriggerRule(&db.TriggerRule{Name: "spawn-reviewer", Enabled: true, OperatorAuthored: true,
		ScopeKind: db.TriggerScopeGroup, GroupID: g.ID, Source: db.TriggerSourcePROpened,
		DraftFilter: db.TriggerDraftExclude, Actions: []db.TriggerAction{{Type: db.TriggerActionSpawn,
			Spawn: &db.TriggerSpawnAction{Profile: "reviewer", NameTemplate: "review-pr-{{pr.number}}",
				InstructionTemplate: "Review {{pr.url}}.", MaxLiveWorkers: 1}}}})
	require.NoError(t, err)
	authorAgent, err := db.AgentIDForConv(author)
	require.NoError(t, err)
	_, err = db.UpsertAgentPRDetails(authorAgent, "https://github.com/o/r/pull/9", "ready", "open", "topic", false)
	require.NoError(t, err)
	agentd.RunTriggerTickForTest(time.Now().Add(time.Second))
	rows, err := db.ListTriggerFirings(ruleID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "spawned", rows[0].Actions[0].Outcome, rows[0].Actions[0].Detail)
	conv, err = db.CurrentConvForAgent(rows[0].Actions[0].SpawnedAgent)
	require.NoError(t, err)
	assert.True(t, convMessagesContain(t, conv, "Follow the review rules."))
	assert.False(t, convMessagesContain(t, conv, "Follow the group rules."), "the profile's set replaces the group's")
}

// removePromptAndProfile simulates a receiving machine that has neither.
func removePromptAndProfile(t *testing.T, f *testharness.Flow, profile, prompt string) {
	t.Helper()
	require.Equal(t, http.StatusNoContent, profileReq(t, f, http.MethodDelete, "/v1/spawn-profiles/"+profile, nil).Code)
	require.Equal(t, http.StatusNoContent, humanReq(t, f, http.MethodDelete, "/v1/saved-prompts/"+prompt, nil).Code)
}

func TestProfileExport_CarriesSavedPrompts(t *testing.T) {
	f := newFlow(t)
	haveSavedPrompts(t, f, map[string]string{"kit-rules": "Follow the kit rules."})
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{"name": "kit", "prompt_refs": []string{"kit-rules"}}).Code)
	rec := profileReq(t, f, http.MethodGet, "/v1/spawn-profiles/export?name=kit", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var env map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Len(t, env["saved_prompts"], 1)

	removePromptAndProfile(t, f, "kit", "kit-rules")
	inspect := profileReq(t, f, http.MethodPost, "/v1/spawn-profiles/import/inspect", env)
	require.Equal(t, http.StatusOK, inspect.Code, inspect.Body.String())
	assert.NotContains(t, inspect.Body.String(), `"valid":false`)
	imp := profileReq(t, f, http.MethodPost, "/v1/spawn-profiles/import", env)
	require.Equal(t, http.StatusOK, imp.Code, imp.Body.String())
	p, err := db.GetSavedPrompt("kit-rules")
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "Follow the kit rules.", p.Body)
}

func TestTemplateExport_CarriesSavedPrompts(t *testing.T) {
	f := newFlow(t)
	haveSavedPrompts(t, f, map[string]string{"kit-rules": "Follow the kit rules."})
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{"name": "kit", "prompt_refs": []string{"kit-rules"}}).Code)
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "kit-team", "agents": []map[string]any{{"name": "worker", "spawn_profile": "kit"}},
	}).Code)
	rec := humanReq(t, f, http.MethodGet, "/v1/templates/kit-team/export", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var env map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Len(t, env["saved_prompts"], 1)

	require.Equal(t, http.StatusNoContent, humanReq(t, f, http.MethodDelete, "/v1/templates/kit-team", nil).Code)
	removePromptAndProfile(t, f, "kit", "kit-rules")
	imp := humanReq(t, f, http.MethodPost, "/v1/templates/import", env)
	require.Equal(t, http.StatusCreated, imp.Code, imp.Body.String())
	p, err := db.GetSpawnProfile("kit")
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, []string{"kit-rules"}, p.PromptRefs)
}

func TestGroupExport_CarriesDefaultPrompts(t *testing.T) {
	f := newFlow(t)
	const conv = "aaaaaaaa-5555-6666-7777-888888888888"
	f.HaveConvWithTitle(conv, "alice")
	f.HaveAliveSession(conv, "lbl-a", "tmux-a", f.TestCwd("src"))
	f.HaveGroup("team")
	f.HaveMember("team", conv)
	haveSavedPrompts(t, f, map[string]string{"team-rules": "Follow the team rules."})
	require.Equal(t, http.StatusOK, humanReq(t, f, http.MethodPatch, "/v1/groups/team", map[string]any{"default_prompts": []string{"team-rules"}}).Code)
	archive := exportGroup(t, f, "team")

	// The receiving library holds a different same-named prompt: it is kept.
	require.Equal(t, http.StatusOK, humanReq(t, f, http.MethodPatch, "/v1/groups/team", map[string]any{"default_prompts": []string{}}).Code)
	require.Equal(t, http.StatusOK, humanReq(t, f, http.MethodPatch, "/v1/saved-prompts/team-rules", map[string]any{"name": "team-rules", "body": "Local rules."}).Code)
	rec := importArchive(f, archive, f.TestCwd("dst"), "team-copy")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "kept the local version")
	g, err := db.GetAgentGroupByName("team-copy")
	require.NoError(t, err)
	assert.Equal(t, []string{"team-rules"}, g.DefaultPrompts)
	p, err := db.GetSavedPrompt("team-rules")
	require.NoError(t, err)
	assert.Equal(t, "Local rules.", p.Body)

	// Absent here, it is recreated from the archive.
	require.Equal(t, http.StatusOK, humanReq(t, f, http.MethodPatch, "/v1/groups/team-copy", map[string]any{"default_prompts": []string{}}).Code)
	require.Equal(t, http.StatusNoContent, humanReq(t, f, http.MethodDelete, "/v1/saved-prompts/team-rules", nil).Code)
	rec = importArchive(f, archive, f.TestCwd("dst2"), "team-copy2")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	p, err = db.GetSavedPrompt("team-rules")
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "Follow the team rules.", p.Body)
}
