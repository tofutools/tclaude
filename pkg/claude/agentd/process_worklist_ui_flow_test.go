package agentd_test

// Flow tests for the Worklist dashboard sub-view's REST consumption (TCL-297).
// The backend derivation/action semantics are covered by the TCL-295 tests in
// process_engine_flow_test.go; these pin the CONTRACT THE UI RIDES — and they
// ride it through the DASHBOARD handler (the popup server's own mux), not the
// daemon socket mux, because that is the surface processes-actions.js fetches.
// The two muxes register routes independently: the first dashsnap run of this
// ticket caught /v1/process/worklist missing from the dashboard mux entirely
// while every daemon-mux test passed. Covered here: the row fields the
// worklist renders (kind, assignee, nudge schedule, created/due, advertised
// actions), the exact request shape processes-actions.js submits (advertised
// action spelling + required comment + fresh idempotency key), that a plain
// re-fetch — the UI does no optimistic update — reflects the resolution, and
// that unreadable runs surface as degradedRuns alongside the healthy items
// (the amber strip's data source, never silently dropped).

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	processmodel "github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/worklist"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// dashboardWorklistReq serves one request against the dashboard popup handler
// (session cookie injected by the test handler, exactly like the browser).
func dashboardWorklistReq(t *testing.T, dash http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(dash, testharness.JSONRequest(t, method, path, body))
}

func TestProcessWorklistUIActionRoundTripReflectsResolution(t *testing.T) {
	_, root := processEngineFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	dash := agentd.BuildDashboardHandlerForTest()
	performer := processmodel.Performer{
		Kind: processmodel.PerformerHuman, Profile: "operator", Ask: "Ship the dashboard release?",
		Contact: &processmodel.ContactSchedule{Cadence: "30m", Budget: 5, EscalationTarget: "human:oncall"},
	}
	createEngineRun(t, root, "worklist-ui-run", decisionTemplate("worklist-ui", performer), false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)

	// The list fetch the sub-view makes (unfiltered — views filter client-side).
	rec := dashboardWorklistReq(t, dash, http.MethodGet, "/v1/process/worklist", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listing struct {
		Items []worklist.Item `json:"items"`
	}
	testharness.DecodeJSON(t, rec, &listing)
	require.Len(t, listing.Items, 1)
	item := listing.Items[0]

	// Every field an item row renders must be present for a live human wait:
	// kind glyph+label, assignee, summary, age, and the nudge schedule line.
	assert.Equal(t, worklist.KindDecisionNeeded, item.Kind)
	assert.Equal(t, "human:operator", item.Assignee)
	assert.Equal(t, "Ship the dashboard release?", item.Summary)
	assert.False(t, item.CreatedAt.IsZero(), "human obligations carry createdAt (the Age cell)")
	assert.Equal(t, item.CreatedAt, item.ChangedAt,
		"a pending item's last change IS its creation (the Recently-changed window)")
	require.NotNil(t, item.Nudge, "the nudge schedule line is the point of the surface")
	assert.False(t, item.Nudge.NextContactAt.IsZero())
	assert.Equal(t, 5, item.Nudge.BudgetMax)
	assert.Equal(t, "human:oncall", item.Nudge.EscalationTarget)
	assert.Equal(t, []string{"approve", "reject"}, item.AvailableActions,
		"the UI renders exactly the advertised actions")
	assert.Equal(t, "worklist-ui-run", item.Links.RunID, "the run deep-link target")
	assert.Equal(t, "decide", item.Links.NodeID)

	// The exact shape processes-actions.js submits (buildWorklistAction): the
	// ADVERTISED action spelling, the trimmed required comment, and a fresh
	// idempotency key minted per click.
	rec = dashboardWorklistReq(t, dash, http.MethodPost, "/v1/process/worklist/"+item.ID+"/action", map[string]string{
		"action":         item.AvailableActions[0],
		"comment":        "reviewed from the worklist tab",
		"idempotencyKey": "b1946ac9-2d47-4f2c-9f2b-worklist-ui1",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The UI does a plain re-fetch after the POST — no optimistic mutation —
	// so the refreshed listing itself must reflect the resolution.
	rec = dashboardWorklistReq(t, dash, http.MethodGet, "/v1/process/worklist", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	testharness.DecodeJSON(t, rec, &listing)
	require.Len(t, listing.Items, 1)
	assert.Equal(t, "satisfied", string(listing.Items[0].Status),
		"refresh after the action must show the item resolved")
	assert.True(t, listing.Items[0].ChangedAt.After(listing.Items[0].CreatedAt),
		"resolution must advance changedAt past creation — the bounded Recently-changed view sorts on it")

	// And the pending-only views (everything except Recently changed) drop it:
	// the status filter the chips would apply server- or client-side agrees.
	rec = dashboardWorklistReq(t, dash, http.MethodGet, "/v1/process/worklist?status=pending", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	testharness.DecodeJSON(t, rec, &listing)
	assert.Empty(t, listing.Items)
}

func TestProcessWorklistUIDegradedRunsSurfaceAlongsideItems(t *testing.T) {
	_, root := processEngineFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	dash := agentd.BuildDashboardHandlerForTest()
	performer := processmodel.Performer{
		Kind: processmodel.PerformerHuman, Profile: "operator", Ask: "Still reviewable?",
	}
	createEngineRun(t, root, "worklist-healthy-run", decisionTemplate("worklist-healthy", performer), false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	for _, corrupt := range []string{"corrupt-alpha", "corrupt-beta"} {
		dir := filepath.Join(root, "runs", corrupt)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "run.json"), []byte("{broken"), 0o644))
	}

	rec := dashboardWorklistReq(t, dash, http.MethodGet, "/v1/process/worklist", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listing struct {
		Items        []worklist.Item `json:"items"`
		DegradedRuns []struct {
			Run   string `json:"run"`
			Error string `json:"error"`
		} `json:"degradedRuns"`
	}
	testharness.DecodeJSON(t, rec, &listing)

	// Healthy items and degraded runs coexist: the strip renders ABOVE the
	// list, it does not replace it.
	require.Len(t, listing.Items, 1)
	assert.Equal(t, "worklist-healthy-run", listing.Items[0].Run)
	require.Len(t, listing.DegradedRuns, 2)
	names := []string{listing.DegradedRuns[0].Run, listing.DegradedRuns[1].Run}
	assert.ElementsMatch(t, []string{"corrupt-alpha", "corrupt-beta"}, names)
	for _, d := range listing.DegradedRuns {
		assert.NotEmpty(t, d.Error, "the strip titles each run with its error")
	}
}
