package agentd_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestEpochV8ExactArtifactPermissionHeadersAndBytes(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	source := func(prompt string) []byte {
		tmpl := &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: "artifact-flow", Start: "work", Nodes: map[string]model.Node{
			"work": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: prompt}, Next: model.Next{"pass": "done"}},
			"done": {Type: model.NodeTypeEnd, Result: "completed"},
		}}
		encoded, encodeErr := model.CanonicalYAML(tmpl)
		require.NoError(t, encodeErr)
		return encoded
	}
	initialSource, nextSource := source("initial-private-prompt"), source("next-private-prompt")
	initialParsed, err := model.ParseExactSource(initialSource)
	require.NoError(t, err)
	initialRecord, err := fs.PutTemplate(t.Context(), initialParsed.Template)
	require.NoError(t, err)
	require.Equal(t, initialParsed.Ref, initialRecord.Ref)
	nextParsed, err := model.ParseExactSource(nextSource)
	require.NoError(t, err)
	nextRecord, err := fs.PutTemplate(t.Context(), nextParsed.Template)
	require.NoError(t, err)
	require.Equal(t, nextParsed.Ref, nextRecord.Ref)
	initialized, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "artifact-flow-run", TemplateRef: initialParsed.Ref}, initialSource)
	require.NoError(t, err)
	classified, err := epochv8.ClassifyTemplateSource(nextSource)
	require.NoError(t, err)
	owner := initialized.Checkpoint.View().Authorities[0].Identity
	preview, err := epochv8.PreviewApply(initialized.Checkpoint, epochv8.ApplyDraft{BaseBinding: initialized.Checkpoint.Binding(), Candidate: classified.Candidate(), Handoffs: []epochv8.HandoffDirective{{Source: owner, Action: epochv8.HandoffTransfer, TargetLocalID: "next-frontier", TargetReservationID: "next-reservation", TargetNodeID: "work"}}})
	require.NoError(t, err)
	require.NotNil(t, preview.Plan)
	fs.SetNowForTest(func() time.Time { return time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC) })
	lease, err := fs.AcquireMaintenanceLease(t.Context(), "artifact-flow-run", "test", time.Minute)
	require.NoError(t, err)
	_, err = fs.PublishEpochV8(t.Context(), lease, preview.Plan, nextSource, nil)
	require.NoError(t, err)
	epochID := preview.Plan.CandidateEpoch().ID
	path := "/v1/process/runs/artifact-flow-run/epochs/" + string(epochID) + "/diff"
	const caller = "artifact-flow-caller-aaaa-bbbb"
	denied := agentReq(t, f, caller, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
	assert.Equal(t, "no-store", denied.Header().Get("Cache-Control"))
	assert.Equal(t, "nosniff", denied.Header().Get("X-Content-Type-Options"))
	missing := agentReq(t, f, caller, http.MethodGet, "/v1/process/runs/missing/epochs/"+strings.Repeat("0", 64)+"/diff", nil)
	assert.Equal(t, denied.Code, missing.Code)
	assert.Equal(t, denied.Body.String(), missing.Body.String())
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermProcessRunsUnlockRead, "test"))
	allowed := agentReq(t, f, caller, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, allowed.Code, allowed.Body.String())
	want, _, err := epochv8.EncodeAppliedEpochDiff(func() *epochv8.CheckpointV8 {
		loaded, loadErr := fs.LoadEpochV8RunView(t.Context(), "artifact-flow-run")
		require.NoError(t, loadErr)
		return loaded.Checkpoint
	}(), epochID)
	require.NoError(t, err)
	assert.Equal(t, want, allowed.Body.Bytes())
	assert.Equal(t, "no-store", allowed.Header().Get("Cache-Control"))
	assert.Equal(t, "nosniff", allowed.Header().Get("X-Content-Type-Options"))
	assert.NotContains(t, allowed.Body.String(), "next-private-prompt")
}
