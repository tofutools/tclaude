package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The Groups › Processes "+ start process" form (G3) posts a run from the
// browser, so the dashboard listener must route POST /v1/process/runs to the
// process runtime (dashboard auth + the processes gate) rather than falling
// through to the SPA's 404.
func TestDashboardRoutesProcessRunCreate(t *testing.T) {
	processAuthoringFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	dash := agentd.BuildDashboardHandlerForTest()

	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost, "/v1/process/runs", map[string]any{
		"templateId": "no-such-template", "authorizeProgramProfiles": []string{},
	}))
	var body struct {
		Code string `json:"code"`
	}
	testharness.DecodeJSON(t, rec, &body)
	assert.Equal(t, "process_run_not_found", body.Code,
		"the process runtime answered (an unknown template), not the SPA fallback: %s", rec.Body.String())
}

// The Groups › Processes single-run focus (G5) reads a run and its events
// and posts a gate's verdict from the browser; each must reach the process
// runtime on the dashboard listener rather than the SPA fallback.
func TestDashboardRoutesProcessRunFocus(t *testing.T) {
	processAuthoringFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	dash := agentd.BuildDashboardHandlerForTest()

	for _, req := range []*http.Request{
		testharness.JSONRequest(t, http.MethodGet, "/v1/process/runs/run_missing", nil),
		testharness.JSONRequest(t, http.MethodGet, "/v1/process/runs/run_missing/events", nil),
		testharness.JSONRequest(t, http.MethodPost, "/v1/process/runs/run_missing/decide", map[string]any{
			"nodeId": "review", "verdict": "approved",
		}),
	} {
		rec := testharness.Serve(dash, req)
		var body struct {
			Code string `json:"code"`
		}
		testharness.DecodeJSON(t, rec, &body)
		assert.Equal(t, "process_run_not_found", body.Code,
			"%s %s answered from the process runtime, not the SPA fallback: %s", req.Method, req.URL.Path, rec.Body.String())
	}
}
