package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type epochV8EnvelopeBody struct {
	Run struct {
		ID              string `json:"id"`
		TemplateRef     string `json:"templateRef"`
		EffectiveStatus string `json:"effectiveStatus"`
	} `json:"run"`
	Schema  string `json:"schema"`
	Adapted bool   `json:"adapted"`
	Lineage struct {
		OriginalTemplateRef string `json:"originalTemplateRef"`
		CurrentTemplateRef  string `json:"currentTemplateRef"`
		TotalEpochs         int    `json:"totalEpochs"`
		Truncated           bool   `json:"truncated"`
		Epochs              []struct {
			Ordinal     uint64 `json:"ordinal"`
			TemplateRef string `json:"templateRef"`
		} `json:"epochs"`
	} `json:"lineage"`
	StructuralSummary struct {
		Nodes               int  `json:"nodes"`
		Edges               int  `json:"edges"`
		ChangedFromOriginal bool `json:"changedFromOriginal"`
	} `json:"structuralSummary"`
	AuthorityCounts struct {
		Total    int `json:"total"`
		Active   int `json:"active"`
		Terminal int `json:"terminal"`
		States   struct {
			VerifiedUnclaimed int `json:"verifiedUnclaimed"`
			Claimed           int `json:"claimed"`
			Active            int `json:"active"`
			Completed         int `json:"completed"`
			Failed            int `json:"failed"`
			Canceled          int `json:"canceled"`
			HandedOff         int `json:"handedOff"`
		} `json:"states"`
	} `json:"authorityCounts"`
	CurrentBinding struct {
		Revision uint64 `json:"revision"`
		Digest   string `json:"digest"`
	} `json:"currentBinding"`
	ViewerV2 struct {
		StateSchemaVersion       int    `json:"stateSchemaVersion"`
		PathProtocol             string `json:"pathProtocol"`
		RoutingAvailable         bool   `json:"routingAvailable"`
		RoutingUnavailableReason string `json:"routingUnavailableReason"`
	} `json:"viewerV2"`
}

func requireProcessNoStoreHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
}

func TestEpochV8SafeEnvelopeSummaryFieldsAndNoStoreHeaders(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)

	tmpl := &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "view-flow",
		Start:      "work",
		Nodes: map[string]model.Node{
			"work": {
				Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "view-private-prompt"},
				Next: model.Next{"pass": "done"},
			},
			"done": {Type: model.NodeTypeEnd, Result: "completed"},
		},
	}
	source, err := model.CanonicalYAML(tmpl)
	require.NoError(t, err)
	parsed, err := model.ParseExactSource(source)
	require.NoError(t, err)
	record, err := fs.PutTemplate(t.Context(), parsed.Template)
	require.NoError(t, err)
	_, err = fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: "view-flow-run", TemplateRef: record.Ref}, source)
	require.NoError(t, err)

	for _, path := range []string{
		"/v1/process/runs/view-flow-run",
		"/v1/process/runs/view-flow-run/view",
	} {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, path, nil)))
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
		requireProcessNoStoreHeaders(t, rec)
		var envelope epochV8EnvelopeBody
		testharness.DecodeJSON(t, rec, &envelope)
		assert.Equal(t, "view-flow-run", envelope.Run.ID, path)
		assert.Equal(t, string(store.RunSchemaEpochV8), envelope.Schema, path)
		assert.False(t, envelope.Adapted, path)
		assert.Equal(t, 1, envelope.Lineage.TotalEpochs, path)
		assert.False(t, envelope.Lineage.Truncated, path)
		require.Len(t, envelope.Lineage.Epochs, 1, path)
		assert.Equal(t, record.Ref, envelope.Lineage.OriginalTemplateRef, path)
		assert.Equal(t, record.Ref, envelope.Lineage.CurrentTemplateRef, path)
		assert.Positive(t, envelope.StructuralSummary.Nodes, path)
		assert.Positive(t, envelope.StructuralSummary.Edges, path)
		assert.False(t, envelope.StructuralSummary.ChangedFromOriginal, path)
		states := envelope.AuthorityCounts.States
		statesTotal := states.VerifiedUnclaimed + states.Claimed + states.Active +
			states.Completed + states.Failed + states.Canceled + states.HandedOff
		assert.Equal(t, envelope.AuthorityCounts.Total, statesTotal, path)
		assert.Equal(t, envelope.AuthorityCounts.Active, states.VerifiedUnclaimed+states.Claimed+states.Active, path)
		assert.Equal(t, 8, envelope.ViewerV2.StateSchemaVersion, path)
		assert.Equal(t, "path_v1_epoch", envelope.ViewerV2.PathProtocol, path)
		assert.False(t, envelope.ViewerV2.RoutingAvailable, path)
		assert.Equal(t, "epoch_v8_summary", envelope.ViewerV2.RoutingUnavailableReason, path)
		// The ordinary envelope must not leak restricted or private material.
		assert.NotContains(t, rec.Body.String(), "view-private-prompt", path)
		assert.NotContains(t, rec.Body.String(), `"authorities"`, path)
		assert.NotContains(t, rec.Body.String(), `"params"`, path)
	}

	verify := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/process/runs/view-flow-run/verify", nil)))
	require.Equal(t, http.StatusOK, verify.Code, verify.Body.String())
	requireProcessNoStoreHeaders(t, verify)
	assert.Contains(t, verify.Body.String(), `"verified":true`)
	assert.Contains(t, verify.Body.String(), `"epoch_v8_summary"`)

	worklist := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/process/worklist", nil)))
	require.Equal(t, http.StatusOK, worklist.Code, worklist.Body.String())
	requireProcessNoStoreHeaders(t, worklist)

	// Error paths keep the exact-content headers: absent runs and stale
	// previews are part of the same no-store contract.
	for _, path := range []string{
		"/v1/process/runs/missing-view-run",
		"/v1/process/runs/missing-view-run/view",
		"/v1/process/runs/missing-view-run/verify",
	} {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, path, nil)))
		require.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", path, rec.Body.String())
		requireProcessNoStoreHeaders(t, rec)
	}
	stale := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/process/runs/view-flow-run/unlock/preview", map[string]any{
			"baseBinding":     map[string]any{"revision": 999, "digest": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
			"candidateSource": string(source),
			"handoffs":        []map[string]any{},
		})))
	require.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	requireProcessNoStoreHeaders(t, stale)
}
