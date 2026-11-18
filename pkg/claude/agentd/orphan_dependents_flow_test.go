package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type orphanedDependentsBody struct {
	Agents []struct {
		AgentID    string `json:"agent_id"`
		State      string `json:"state"`
		Title      string `json:"title"`
		Dependents []struct {
			ID int64 `json:"id"`
		} `json:"dependents"`
		Runs []struct {
			ID string `json:"id"`
		} `json:"runs"`
		Result *struct {
			Removed []struct {
				ID int64 `json:"id"`
			} `json:"removed"`
			RunsCancelled []struct {
				ID string `json:"id"`
			} `json:"runs_cancelled"`
		} `json:"result"`
	} `json:"agents"`
}

// Scenario: an agent retired with --keep-dependents leaves its cron job and
// its scoped run behind. The Doctor counts them; `cleanup-dependents
// --orphaned --dry-run` lists them without touching them; the cleanup
// removes the job and cancels the run. Only the operator may do either.
func TestOrphanedDependents_DryRunThenCleanup(t *testing.T) {
	f, root := processRuntimeFlow(t)
	const conv = "orph-1111-2222-3333-4444"
	agentID := havePerformerAgent(t, f, "", conv, "left-behind", "", "offline")
	putProcessRuntimeTemplate(t, root, performerTemplate("orphan-template", devPerformer(), nil))
	byName, byID := devPerformer(), devPerformer()
	byName.Assignee, byID.Assignee = "left-behind", agentID
	putProcessRuntimeTemplate(t, root, performerTemplate("pinned-by-name", byName, nil))
	putProcessRuntimeTemplate(t, root, performerTemplate("pinned-by-id", byID, nil))
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
		"id": "run_orphan", "templateId": "orphan-template", "agent": agentID, "authorizeProgramProfiles": []string{},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	agentd.WaitForProcessDispatchForTest()
	cron, err := db.InsertAgentCronJob(&db.AgentCronJob{Name: "nudge", OwnerConv: conv, TargetConv: conv,
		TargetKind: db.CronTargetConv, IntervalSeconds: 600, Body: "nudge", Enabled: true})
	require.NoError(t, err)

	retired := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/agent/"+conv+"/retire?shutdown=0&delete_worktree=0&keep_dependents=1", nil)))
	require.Equal(t, http.StatusOK, retired.Code, retired.Body.String())
	assert.Contains(t, agentd.OrphanedDependentsCheckTitleForTest(), "1 retired or deleted agent")

	const bystander = "orpb-1111-2222-3333-4444"
	havePerformerAgent(t, f, "", bystander, "bystander", "", "offline")
	denied := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/orphaned-dependents", nil), bystander))
	assert.Equal(t, http.StatusForbidden, denied.Code, "an agent may not clean up other agents' rows")

	preview := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/orphaned-dependents", nil)))
	require.Equal(t, http.StatusOK, preview.Code, preview.Body.String())
	var listed orphanedDependentsBody
	testharness.DecodeJSON(t, preview, &listed)
	require.Len(t, listed.Agents, 1)
	assert.Equal(t, agentID, listed.Agents[0].AgentID)
	assert.Equal(t, "retired", listed.Agents[0].State)
	require.Len(t, listed.Agents[0].Dependents, 1)
	assert.Equal(t, cron, listed.Agents[0].Dependents[0].ID)
	require.Len(t, listed.Agents[0].Runs, 1)
	job, err := db.GetAgentCronJob(cron)
	require.NoError(t, err)
	require.NotNil(t, job, "the dry run leaves the job")
	assert.NotEqual(t, engine.RunCanceled, showPerformerRun(t, f, "run_orphan").Status, "the dry run leaves the run")

	cleaned := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/orphaned-dependents", nil)))
	require.Equal(t, http.StatusOK, cleaned.Code, cleaned.Body.String())
	var done orphanedDependentsBody
	testharness.DecodeJSON(t, cleaned, &done)
	require.Len(t, done.Agents, 1)
	require.NotNil(t, done.Agents[0].Result)
	require.Len(t, done.Agents[0].Result.Removed, 1)
	require.Len(t, done.Agents[0].Result.RunsCancelled, 1)
	agentd.WaitForProcessDispatchForTest()
	job, err = db.GetAgentCronJob(cron)
	require.NoError(t, err)
	assert.Nil(t, job)
	assert.Equal(t, engine.RunCanceled, showPerformerRun(t, f, "run_orphan").Status)
	assert.Empty(t, agentd.OrphanedDependentsCheckTitleForTest(), "nothing is left for the Doctor to report")

	fs, err := store.NewFS(root)
	require.NoError(t, err)
	assignee := func(id string) string {
		head, err := fs.GetTemplateHeadGeneration(t.Context(), id)
		require.NoError(t, err)
		current, err := fs.GetTemplate(t.Context(), head.Ref)
		require.NoError(t, err)
		return current.Nodes[performerNode].Performer.Assignee
	}
	assert.Empty(t, assignee("pinned-by-id"), "a step pinned by the agent's id is cleared")
	assert.Equal(t, "left-behind", assignee("pinned-by-name"),
		"a step pinned by a retired agent's name is left: a later agent may answer to it")
}
