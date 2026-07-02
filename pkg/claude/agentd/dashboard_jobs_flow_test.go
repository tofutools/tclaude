package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// jobsPage decodes the /api/jobs envelope — the /api/retired family's shared
// shape with the unified {kind, export?, cron?} rows.
type jobsPage struct {
	Rows []struct {
		Kind   string `json:"kind"`
		Export *struct {
			ID     int64  `json:"id"`
			Title  string `json:"title"`
			Status string `json:"status"`
		} `json:"export"`
		Cron *struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"cron"`
	} `json:"rows"`
	Offset          int `json:"offset"`
	Limit           int `json:"limit"`
	Total           int `json:"total"`
	TotalUnfiltered int `json:"total_unfiltered"`
}

func getJobs(t *testing.T, dash http.Handler, query string) jobsPage {
	t.Helper()
	rec := testharness.Serve(dash, dashReq(t, http.MethodGet, "/api/jobs"+query, nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var page jobsPage
	testharness.DecodeJSON(t, rec, &page)
	return page
}

// TestDashboardJobs_UnifiedListing exercises GET /api/jobs — the Jobs tab's
// unified export + cron listing: kind discrimination, the server-side q
// filter (it searches the WHOLE set, not a page), offset/limit windowing and
// stale-offset clamping (the /api/retired contract).
func TestDashboardJobs_UnifiedListing(t *testing.T) {
	newFlow(t)
	dash := agentd.BuildDashboardHandlerForTest()

	_, err := db.InsertExportJob(&db.ExportJob{
		ConvID: "jjj00000-0000-4000-8000-000000000001", Title: "alpha report",
		Status: db.ExportStatusCloning,
	})
	require.NoError(t, err)
	_, err = db.InsertExportJob(&db.ExportJob{
		ConvID: "jjj00000-0000-4000-8000-000000000002", Title: "beta summary",
		Status: db.ExportStatusRunning,
	})
	require.NoError(t, err)
	_, err = db.InsertAgentCronJob(&db.AgentCronJob{
		Name: "gamma-ping", TargetKind: db.CronTargetConv,
		TargetConv:      "jjj00000-0000-4000-8000-000000000001",
		IntervalSeconds: 300, Subject: "ping", Body: "status?", Enabled: true,
	})
	require.NoError(t, err)

	// Unfiltered: every job of both kinds, one list.
	all := getJobs(t, dash, "")
	assert.Equal(t, 3, all.Total)
	assert.Equal(t, 3, all.TotalUnfiltered)
	require.Len(t, all.Rows, 3)
	kinds := map[string]int{}
	for _, r := range all.Rows {
		kinds[r.Kind]++
		switch r.Kind {
		case "export":
			require.NotNil(t, r.Export, "an export row must carry its export payload")
			assert.Nil(t, r.Cron)
		case "cron":
			require.NotNil(t, r.Cron, "a cron row must carry its cron payload")
			assert.Nil(t, r.Export)
			assert.Equal(t, "gamma-ping", r.Cron.Name)
		}
	}
	assert.Equal(t, map[string]int{"export": 2, "cron": 1}, kinds)

	// The q filter is server-side and spans both kinds' text fields.
	cronOnly := getJobs(t, dash, "?q=gamma")
	assert.Equal(t, 1, cronOnly.Total)
	assert.Equal(t, 3, cronOnly.TotalUnfiltered, "total_unfiltered ignores q")
	require.Len(t, cronOnly.Rows, 1)
	assert.Equal(t, "cron", cronOnly.Rows[0].Kind)

	exportOnly := getJobs(t, dash, "?q=alpha")
	require.Len(t, exportOnly.Rows, 1)
	require.Equal(t, "export", exportOnly.Rows[0].Kind)
	assert.Equal(t, "alpha report", exportOnly.Rows[0].Export.Title)

	// A kind name matches as filter text too — "export" finds both exports.
	byKind := getJobs(t, dash, "?q=export")
	assert.Equal(t, 2, byKind.Total)

	// Windowing: limit bounds the page, total still counts everything.
	page1 := getJobs(t, dash, "?offset=0&limit=2")
	assert.Len(t, page1.Rows, 2)
	assert.Equal(t, 3, page1.Total)
	assert.Equal(t, 0, page1.Offset)

	// A stale offset past the end is clamped back to the last page.
	clamped := getJobs(t, dash, "?offset=99&limit=2")
	assert.Equal(t, 2, clamped.Offset, "stale offset clamps to the last page start")
	assert.Len(t, clamped.Rows, 1)
}
