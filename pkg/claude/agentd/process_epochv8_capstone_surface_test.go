package agentd_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

// Scenario: schema-8 runs serve one safe summary envelope from both
// GET /v1/process/runs/{id} and its /view sibling through the shared
// epochV8SafeEnvelope builder, and every denial on the sensitive mutation
// routes stays attributable and non-cacheable. One seeded run backs all three
// contracts:
//
//   - the two summary routes answer with byte-identical envelopes and
//     identical no-store headers (a divergence would mean one route grew or
//     lost fields relative to the shared builder);
//   - a DENIED unlock apply from an unprivileged agent still writes an audit
//     row attributing the denied caller with the templated path and none of
//     the request's candidate/reason material;
//   - the settlement route denial carries the same no-store/nosniff contract,
//     pinned to headers being set before the permission gate.
func TestEpochV8CapstoneCrossRouteEqualityAndDenialSurfaces(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, err := store.NewFS(root)
	require.NoError(t, err)

	tmpl := &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "capstone-surface",
		Start:      "work",
		Nodes: map[string]model.Node{
			"work": {
				Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "capstone-private-prompt"},
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
	const runID = "capstone-surface-run"
	initialized, err := fs.InitializeEpochV8Run(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, source)
	require.NoError(t, err)

	t.Run("cross-route safe envelope equality", func(t *testing.T) {
		get := processEngineGet(t, f, "/v1/process/runs/"+runID)
		view := processEngineGet(t, f, "/v1/process/runs/"+runID+"/view")
		require.Equal(t, http.StatusOK, get.Code, get.Body.String())
		require.Equal(t, http.StatusOK, view.Code, view.Body.String())
		requireProcessNoStoreHeaders(t, get)
		requireProcessNoStoreHeaders(t, view)
		for _, header := range []string{"Cache-Control", "X-Content-Type-Options"} {
			assert.Equal(t, get.Header().Values(header), view.Header().Values(header), header)
		}

		// Both routes flow through the shared builder with the same snapshot,
		// so the serialized envelopes must be literally identical — first as
		// raw bytes, then re-checked structurally and over canonicalized bytes
		// so a future encoding-only divergence still reports which field moved.
		assert.Equal(t, get.Body.String(), view.Body.String(), "summary and view route bodies diverged")
		var getBody, viewBody any
		require.NoError(t, json.Unmarshal(get.Body.Bytes(), &getBody))
		require.NoError(t, json.Unmarshal(view.Body.Bytes(), &viewBody))
		assert.True(t, reflect.DeepEqual(getBody, viewBody), "normalized envelopes diverged:\nGET: %s\nview: %s", get.Body.String(), view.Body.String())
		getCanonical, err := json.Marshal(getBody)
		require.NoError(t, err)
		viewCanonical, err := json.Marshal(viewBody)
		require.NoError(t, err)
		assert.Equal(t, string(getCanonical), string(viewCanonical))
	})

	const caller = "capstone-denied-caller-11111111"
	f.HaveConvWithTitle(caller, "capstone-denied-caller")
	_, _, err = db.EnsureAgentForConv(caller, "test")
	require.NoError(t, err)

	t.Run("denied apply writes attributed audit row without request material", func(t *testing.T) {
		denied := agentReq(t, f, caller, http.MethodPost, "/v1/process/runs/"+runID+"/unlock/apply", map[string]any{
			"baseBinding":     map[string]any{"revision": 1, "digest": "capstone-query-sentinel"},
			"candidateSource": "capstone-candidate-sentinel",
			"reason":          "capstone-reason-sentinel",
		})
		require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
		requireProcessNoStoreHeaders(t, denied)

		audits, err := db.ListAuditLog(db.AuditLogFilter{Verb: "process.unlock.apply", Outcome: "failure"})
		require.NoError(t, err)
		require.Len(t, audits, 1, "denied unlock apply must leave exactly one audit row")
		row := audits[0]
		assert.Equal(t, db.AuditActorAgent, row.ActorKind)
		assert.Equal(t, caller, row.ActorConv)
		assert.Equal(t, "capstone-denied-caller", row.ActorLabel)
		assert.Equal(t, http.MethodPost, row.Method)
		assert.Equal(t, http.StatusForbidden, row.Status)
		assert.Equal(t, "/v1/process/runs/{id}/unlock/apply", row.Path)
		// The unlock-apply describer is deliberately nil and the denial happens
		// before body decoding: nothing from the request body or the concrete
		// run id may reach the trail.
		assert.Empty(t, row.Detail)
		for _, forbidden := range []string{"capstone-candidate-sentinel", "capstone-reason-sentinel", "capstone-query-sentinel", runID} {
			assert.NotContains(t, row.Detail, forbidden)
			assert.NotContains(t, row.Path, forbidden)
		}
	})

	t.Run("settlement denial keeps no-store headers", func(t *testing.T) {
		denied := agentReq(t, f, caller, http.MethodPost, "/v1/process/runs/"+runID+"/unblock", map[string]any{
			"decision": "retry", "reason": "capstone-settlement-sentinel",
		})
		require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
		requireProcessNoStoreHeaders(t, denied)
	})

	// None of the denied mutations may have moved the run.
	after, err := fs.LoadEpochV8RunView(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, initialized.Checkpoint.Binding(), after.Checkpoint.Binding())
}
