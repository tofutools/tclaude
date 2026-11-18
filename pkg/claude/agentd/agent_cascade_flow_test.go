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

// Scenario: retiring an agent cancels the run scoped to it and clears the
// template step pinned to it, saved as a new version that says why. The
// dialogs' preview listed both beforehand. Kept, both are left as they were.
func TestAgentCascade_RetireCancelsScopedRunAndClearsPinnedStep(t *testing.T) {
	for _, keep := range []bool{false, true} {
		name := "cascade"
		if keep {
			name = "keep"
		}
		t.Run(name, func(t *testing.T) {
			f, root := processRuntimeFlow(t)
			const conv = "csc0-1111-2222-3333-4444"
			agentID := havePerformerAgent(t, f, "", conv, "pinned-worker", "", "offline")

			pinned := devPerformer()
			pinned.Assignee = "pinned-worker"
			putProcessRuntimeTemplate(t, root, performerTemplate("pinned-template", pinned, nil))
			putProcessRuntimeTemplate(t, root, performerTemplate("scoped-template", devPerformer(), nil))
			created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", map[string]any{
				"id": "run_scoped", "templateId": "scoped-template", "agent": agentID, "authorizeProgramProfiles": []string{},
			})
			require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
			agentd.WaitForProcessDispatchForTest()
			require.Equal(t, engine.RunRunning, showPerformerRun(t, f, "run_scoped").Status)

			t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			previewReq := testharness.JSONRequest(t, http.MethodGet, "/api/agents/"+conv+"/dependents", nil)
			previewReq.Header.Set("Origin", "http://127.0.0.1:0")
			preview := testharness.Serve(agentd.BuildDashboardHandlerForTest(), previewReq)
			require.Equal(t, http.StatusOK, preview.Code, preview.Body.String())
			var listed struct {
				Runs []struct {
					ID string `json:"id"`
				} `json:"runs"`
				PinnedSteps []struct {
					Template string `json:"template"`
					Node     string `json:"node"`
				} `json:"pinned_steps"`
			}
			testharness.DecodeJSON(t, preview, &listed)
			require.Len(t, listed.Runs, 1)
			assert.Equal(t, "run_scoped", listed.Runs[0].ID)
			require.Len(t, listed.PinnedSteps, 1)
			assert.Equal(t, "pinned-template", listed.PinnedSteps[0].Template)

			path := "/v1/agent/" + conv + "/retire?shutdown=0&delete_worktree=0"
			if keep {
				path += "&keep_dependents=1"
			}
			rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, path, nil)))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp struct {
				Outcome struct {
					Dependents struct {
						Kept          bool `json:"kept"`
						RunsCancelled []struct {
							ID string `json:"id"`
						} `json:"runs_cancelled"`
						StepsCleared []struct {
							Template string `json:"template"`
						} `json:"steps_cleared"`
					} `json:"dependents"`
				} `json:"outcome"`
			}
			testharness.DecodeJSON(t, rec, &resp)
			agentd.WaitForProcessDispatchForTest()

			fs, err := store.NewFS(root)
			require.NoError(t, err)
			head, err := fs.GetTemplateHeadGeneration(t.Context(), "pinned-template")
			require.NoError(t, err)
			current, err := fs.GetTemplate(t.Context(), head.Ref)
			require.NoError(t, err)
			after := showPerformerRun(t, f, "run_scoped")

			if keep {
				assert.True(t, resp.Outcome.Dependents.Kept)
				assert.Empty(t, resp.Outcome.Dependents.RunsCancelled)
				assert.Equal(t, "pinned-worker", current.Nodes[performerNode].Performer.Assignee, "a kept retire leaves the pinned step")
				assert.NotEqual(t, engine.RunCanceled, after.Status, "a kept retire leaves the run parked")
				return
			}
			require.Len(t, resp.Outcome.Dependents.RunsCancelled, 1)
			require.Len(t, resp.Outcome.Dependents.StepsCleared, 1)
			assert.Equal(t, engine.RunCanceled, after.Status)
			assert.Empty(t, current.Nodes[performerNode].Performer.Assignee, "the pinned step goes back to nobody yet")
			authorship, err := fs.ListTemplateAuthorship(t.Context(), head.Ref)
			require.NoError(t, err)
			require.NotEmpty(t, authorship)
			last := authorship[len(authorship)-1]
			assert.Equal(t, store.ActorRef("human:operator"), last.Actor)
			assert.Equal(t, "cleared pinned-worker after it was retired", last.Note)
			ok, err := db.IsLiveAgentConv(conv)
			require.NoError(t, err)
			assert.False(t, ok)
		})
	}
}
