package browser

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserProcessMonitorShowsDurableNodesAndRefreshesCancellation(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "pause", Nodes: []model.WorkNode{{ID: "pause", Name: "Await scheduled time", Kind: model.WorkNodeWait, Wait: &model.WaitPolicy{Duration: time.Hour}}, {ID: "done", Name: "Finish", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "pause", To: "done"}}}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/processes", map[string]any{"request_id": "start_monitor", "id": "monitor_run", "start": model.WorkStart{InlineGraph: &graph, Deadline: time.Now().Add(2 * time.Hour)}}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElement("[data-tab=work]").MustClick()
	page.MustElementR("#work-list button", "^Inspect process graph$").MustClick()
	page.MustElement(".process-monitor [aria-label='Process execution graph']")
	page.MustElement(".process-monitor .process-node[data-node-id='pause']").MustClick()
	page.MustElementR(".process-monitor aside h3", "Await scheduled time")
	page.MustElementR(".process-monitor aside h4", "Attempt 1")
	page.MustElement(".process-monitor .process-node[data-node-id='done']").MustClick()
	page.MustElementR(".process-monitor aside", "This node has not been activated")
	var current struct {
		Run struct {
			Revision model.Revision `json:"revision"`
		} `json:"run"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/work/monitor_run", nil, &current))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/work/cancel", map[string]any{"request_id": "cancel_monitor", "work_run_id": "monitor_run", "expected_revision": current.Run.Revision, "reason": "operator cancelled wait"}, nil))
	page.MustElementR(".process-monitor button", "^Refresh process$").MustClick()
	page.MustElementR(".process-monitor > p", "cancelled")
	page.MustElementR(".process-monitor button", "^Close process$").MustClick()
	require.Eventually(t, func() bool { return !page.MustHas(".process-monitor") }, time.Second, 10*time.Millisecond)
	require.False(t, page.MustElement("#error").MustVisible())
}
