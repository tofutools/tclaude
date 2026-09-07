package browser

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
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

func TestBrowserProcessMonitorInspectsAnsweredDecision(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "review", Nodes: []model.WorkNode{{ID: "review", Name: "Review result", Kind: model.WorkNodeDecision, Decision: &model.DecisionNode{Kind: model.DecisionWork, Audience: []model.DecisionAudience{{Subject: model.AuthoritySubject{Kind: model.AuthorityOperator}}}, PermittedAnswers: []string{"approve"}, ExpiresAfter: time.Hour}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "review", To: "done", Verdict: "approve"}}}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/processes", map[string]any{"request_id": "decision_monitor", "id": "decision_monitor", "start": model.WorkStart{InlineGraph: &graph, Deadline: time.Now().Add(time.Hour)}}, nil))
	var windows []app.DecisionResult
	require.Eventually(t, func() bool {
		return operator.Call(ctx, "GET", "/v2/decisions", nil, &windows) == nil && len(windows) == 1
	}, 10*time.Second, 20*time.Millisecond)
	window := windows[0].Window
	require.NoError(t, operator.Call(ctx, "POST", "/v2/decisions/submit", map[string]any{"request_id": "answer", "decision_id": window.ID, "expected_window_revision": window.Revision, "answer": "approve", "reason": "Verified the expected artifact"}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElement("[data-tab=work]").MustClick()
	page.MustElementR("#work-list button", "^Inspect process graph$").MustClick()
	page.MustElement(".process-monitor .process-node[data-node-id='review']").MustClick()
	page.MustElementR(".process-monitor aside button", "^Inspect decision$").MustClick()
	page.MustElementR(".process-monitor aside", "Answer: approve")
	page.MustElementR(".process-monitor aside", "Verified the expected artifact")
	page.MustElementR(".process-monitor aside", "Decided by operator")
	require.False(t, page.MustHas(".process-monitor aside button[disabled]"))
	page.MustElementR(".process-monitor button", "^Close process$").MustClick()
}
