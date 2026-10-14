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
