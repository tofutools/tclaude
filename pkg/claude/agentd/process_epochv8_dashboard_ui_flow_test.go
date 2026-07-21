package agentd_test

// The S6 unlock panel and exact-artifact drill-down fetch through the
// DASHBOARD handler (its own mux), not the daemon socket mux. These tests pin
// the contract the UI rides — registration, real dashboard auth stamping, the
// permission behavior of the human operator, and the no-store headers — end
// to end through agentd.BuildDashboardHandlerForTest. JS stub tests cannot
// catch a route missing from the dashboard mux; this file exists because the
// cold review caught exactly that.

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestEpochV8DashboardMuxServesPreviewApplyAndArtifact(t *testing.T) {
	_, root := processEngineFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	dash := agentd.BuildDashboardHandlerForTest()
	fs, err := store.NewFS(root)
	require.NoError(t, err)

	source := func(prompt string) []byte {
		tmpl := &model.Template{
			APIVersion: model.APIVersion, Kind: model.Kind, ID: "dash-unlock", Start: "work",
			Nodes: map[string]model.Node{
				"work": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: prompt}, Next: model.Next{"pass": "done"}},
				"done": {Type: model.NodeTypeEnd, Result: "completed"},
			},
		}
		encoded, encodeErr := model.CanonicalYAML(tmpl)
		require.NoError(t, encodeErr)
		return encoded
	}
	initialSource, candidateSource := source("dash-initial-private"), source("dash-candidate-private")
	parsed, err := model.ParseExactSource(initialSource)
	require.NoError(t, err)
	record, err := fs.PutTemplate(t.Context(), parsed.Template)
	require.NoError(t, err)
	initialized, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "dash-unlock-run", TemplateRef: record.Ref}, initialSource)
	require.NoError(t, err)
	base := map[string]any{
		"revision": initialized.Checkpoint.Binding().Revision,
		"digest":   initialized.Checkpoint.Binding().Digest,
	}

	// Preview rides the dashboard mux as the stamped human operator: blocked
	// first (a handoff is required), then valid with the retain directive.
	blocked := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/v1/process/runs/dash-unlock-run/unlock/preview", map[string]any{
			"baseBinding": base, "candidateSource": string(candidateSource), "handoffs": []map[string]any{},
		}))
	require.Equal(t, http.StatusUnprocessableEntity, blocked.Code, blocked.Body.String())
	requireProcessNoStoreHeaders(t, blocked)
	var blockedBody struct {
		Blockers []struct {
			Token          string   `json:"token"`
			HandoffClass   string   `json:"handoffClass"`
			AllowedActions []string `json:"allowedActions"`
		} `json:"blockers"`
	}
	testharness.DecodeJSON(t, blocked, &blockedBody)
	require.Len(t, blockedBody.Blockers, 1)
	handoff := map[string]any{"token": blockedBody.Blockers[0].Token, "action": epochv8.HandoffRetain}
	valid := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/v1/process/runs/dash-unlock-run/unlock/preview", map[string]any{
			"baseBinding": base, "candidateSource": string(candidateSource), "reason": "dash-restricted-reason",
			"handoffs": []map[string]any{handoff},
		}))
	require.Equal(t, http.StatusOK, valid.Code, valid.Body.String())
	requireProcessNoStoreHeaders(t, valid)
	var previewBody struct {
		ApplyToken string `json:"applyToken"`
	}
	testharness.DecodeJSON(t, valid, &previewBody)
	require.Len(t, previewBody.ApplyToken, 64)

	// Apply as the dashboard human operator passes the process.runs.unlock
	// permission check (classHuman) without a popup and mints the epoch.
	applied := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/v1/process/runs/dash-unlock-run/unlock/apply", map[string]any{
			"baseBinding": base, "applyToken": previewBody.ApplyToken,
			"candidateSource": string(candidateSource), "reason": "dash-restricted-reason",
			"handoffs": []map[string]any{handoff},
		}))
	require.Equal(t, http.StatusOK, applied.Code, applied.Body.String())
	requireProcessNoStoreHeaders(t, applied)
	var appliedBody struct {
		Status  string `json:"status"`
		EpochID string `json:"epochId"`
		Actor   string `json:"actor"`
	}
	testharness.DecodeJSON(t, applied, &appliedBody)
	assert.Equal(t, "applied", appliedBody.Status)
	assert.Equal(t, "human:operator", appliedBody.Actor)
	require.Len(t, appliedBody.EpochID, 64)

	// The exact artifact drill-down reads the applied epoch's diff and reason
	// through the dashboard mux with the no-store contract; the human
	// operator passes process.runs.unlock.read.
	for _, kind := range []string{"diff", "reason"} {
		rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet,
			"/v1/process/runs/dash-unlock-run/epochs/"+appliedBody.EpochID+"/"+kind, nil))
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", kind, rec.Body.String())
		requireProcessNoStoreHeaders(t, rec)
		require.NotEmpty(t, rec.Body.String(), kind)
	}
	reason := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet,
		"/v1/process/runs/dash-unlock-run/epochs/"+appliedBody.EpochID+"/reason", nil))
	assert.Equal(t, "dash-restricted-reason", reason.Body.String())
}
