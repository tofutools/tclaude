package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	processengine "github.com/tofutools/tclaude/pkg/claude/process/engine"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
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

	// The runs listing carries schema-8 adapted state, so it shares the
	// no-store contract too.
	runsList := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/process/runs", nil)))
	require.Equal(t, http.StatusOK, runsList.Code, runsList.Body.String())
	requireProcessNoStoreHeaders(t, runsList)

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

func TestEpochV8WorklistOwnerEpochItemsReportAgreementAndFailClosed(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)

	tmpl := &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: "epoch-worklist-flow", Start: "hold", Nodes: map[string]model.Node{
		"hold": {Type: model.NodeTypeWait, Wait: &model.WaitConfig{Signal: "go-ahead"}, Next: model.Next{"pass": "done"}},
		"done": {Type: model.NodeTypeEnd, Result: "completed"},
	}}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	created := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateRef": record.Ref, "runId": "epoch-worklist-flow",
	})))
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())

	// Before the runtime attaches, the run truthfully has zero items and is
	// not degraded.
	empty := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/process/worklist?run=epoch-worklist-flow", nil)))
	require.Equal(t, http.StatusOK, empty.Code, empty.Body.String())
	requireProcessNoStoreHeaders(t, empty)
	assert.NotContains(t, empty.Body.String(), "epoch_v8_incoherent")
	assert.NotContains(t, empty.Body.String(), `"ownerEpoch"`)

	host := processengine.New(fs, "agentd:epoch-worklist", map[model.PerformerKind]processexec.Adapter{})
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)

	type worklistBody struct {
		Items []struct {
			ID         string `json:"id"`
			Run        string `json:"run"`
			Node       string `json:"node"`
			Attempt    int    `json:"attempt"`
			Kind       string `json:"kind"`
			Assignee   string `json:"assignee"`
			Status     string `json:"status"`
			Summary    string `json:"summary"`
			OwnerEpoch *struct {
				Ordinal     uint64 `json:"ordinal"`
				TemplateRef string `json:"templateRef"`
			} `json:"ownerEpoch"`
		} `json:"items"`
		DegradedRuns []struct {
			Run   string `json:"run"`
			Error string `json:"error"`
		} `json:"degradedRuns"`
	}
	fetch := func() worklistBody {
		t.Helper()
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/process/worklist?run=epoch-worklist-flow", nil)))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		requireProcessNoStoreHeaders(t, rec)
		var body worklistBody
		testharness.DecodeJSON(t, rec, &body)
		return body
	}
	first := fetch()
	require.Len(t, first.Items, 1, "the pending signal wait is the one outstanding item")
	item := first.Items[0]
	assert.True(t, strings.HasPrefix(item.ID, "wi8_"), item.ID)
	assert.Equal(t, "epoch-worklist-flow", item.Run)
	assert.Equal(t, "hold", item.Node)
	assert.Equal(t, "waiting", item.Kind)
	assert.Equal(t, "pending", item.Status)
	assert.Equal(t, "Waiting for signal go-ahead", item.Summary)
	assert.Empty(t, item.Assignee)
	require.NotNil(t, item.OwnerEpoch, "schema-8 items resolve through their owner epoch")
	assert.Equal(t, uint64(0), item.OwnerEpoch.Ordinal)
	assert.Equal(t, record.Ref, item.OwnerEpoch.TemplateRef)
	assert.Empty(t, first.DegradedRuns)

	// Determinism: an unchanged coherent checkpoint re-projects identically.
	second := fetch()
	require.Len(t, second.Items, 1)
	assert.Equal(t, item.ID, second.Items[0].ID)

	// The envelope report shares the projection core: same key, same state.
	view := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/process/runs/epoch-worklist-flow/view", nil)))
	require.Equal(t, http.StatusOK, view.Code, view.Body.String())
	var viewBody struct {
		EpochReport struct {
			Entries []struct {
				ID                string `json:"id"`
				OwnerEpochOrdinal uint64 `json:"ownerEpochOrdinal"`
				Kind              string `json:"kind"`
				NodeID            string `json:"nodeId"`
				Attempt           int    `json:"attempt"`
				Status            string `json:"status"`
			} `json:"entries"`
			TimelineTotal int `json:"timelineTotal"`
		} `json:"epochReport"`
	}
	testharness.DecodeJSON(t, view, &viewBody)
	require.Len(t, viewBody.EpochReport.Entries, 1)
	entry := viewBody.EpochReport.Entries[0]
	assert.Equal(t, item.ID, entry.ID)
	assert.Equal(t, uint64(0), entry.OwnerEpochOrdinal)
	assert.Equal(t, item.Kind, entry.Kind)
	assert.Equal(t, item.Node, entry.NodeID)
	assert.Equal(t, item.Attempt, entry.Attempt)
	assert.Equal(t, item.Status, entry.Status)
	// The engine's runtime genesis and wait scheduling are on the timeline.
	assert.Positive(t, viewBody.EpochReport.TimelineTotal)

	// Satisfying the signal settles the wait; the item leaves pending state.
	signal := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/process/runs/epoch-worklist-flow/nodes/hold/signal", map[string]any{"signal": "go-ahead"})))
	require.Equal(t, http.StatusOK, signal.Code, signal.Body.String())
	after := fetch()
	for _, remaining := range after.Items {
		assert.NotEqual(t, "pending", remaining.Status)
	}

	// Tampering with the owner epoch source is a whole-run coherence failure:
	// zero partial items, one bounded degraded code, and no relabeling.
	snapshot, err := fs.LoadEpochV8RunView(t.Context(), "epoch-worklist-flow")
	require.NoError(t, err)
	epochID := snapshot.Checkpoint.View().Epochs[0].ID
	sourcePath := filepath.Join(root, "runs", "epoch-worklist-flow", "epochs", string(epochID), "source.yaml")
	require.NoError(t, os.WriteFile(sourcePath, []byte("tampered: true\n"), 0o644))
	degraded := fetch()
	assert.Empty(t, degraded.Items, "a tampered owner source must never yield partial or relabeled items")
	require.Len(t, degraded.DegradedRuns, 1)
	assert.Equal(t, "epoch-worklist-flow", degraded.DegradedRuns[0].Run)
	assert.Equal(t, "epoch_v8_incoherent", degraded.DegradedRuns[0].Error)
	brokenView := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/process/runs/epoch-worklist-flow/view", nil)))
	require.NotEqual(t, http.StatusOK, brokenView.Code)
	requireProcessNoStoreHeaders(t, brokenView)
	assert.NotContains(t, brokenView.Body.String(), "tampered")
}
