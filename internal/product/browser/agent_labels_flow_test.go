package browser

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserAgentLabelsEditCopyAndClearLiterally(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	desired := model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "source", "name": "Source", "desired": desired}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustElementR("#roster button", "^Configure$").MustClick()
	page.MustElement("#editor [name=role_label]").MustInput("engineer")
	page.MustElement("#editor [name=description]").MustInput("Literal <b>description</b>\nSecond line")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#roster", "Literal <b>description</b>")
	require.False(t, page.MustEval(`() => !!document.querySelector('#roster b')`).Bool())
	page.MustReload()
	page.MustElementR("#connection", "^Updated ")
	page.MustElementR("#roster button", "^Configure$").MustClick()
	require.Equal(t, "engineer", page.MustElement("#editor [name=role_label]").MustProperty("value").Str())
	require.Equal(t, "Literal <b>description</b>\nSecond line", page.MustElement("#editor [name=description]").MustProperty("value").Str())
	page.MustEval(`() => document.querySelector('#editor').close()`)
	page.MustElementR("#roster button", "^Clone configuration$").MustClick()
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var snapshot struct {
		Agents     []model.Agent     `json:"agents"`
		Executions []model.Execution `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 2)
	require.Empty(t, snapshot.Executions)
	for _, agent := range snapshot.Agents {
		require.Equal(t, model.AgentLabels{Role: "engineer", Description: "Literal <b>description</b>\nSecond line"}, agent.Labels)
	}
	// Select the source by its visible name, then clear both fields explicitly.
	page.MustEval(`() => {const row=Array.from(document.querySelectorAll('#roster .row')).find(row=>row.textContent.includes('Source')&&!row.textContent.includes('Source copy'));if(row)Array.from(row.querySelectorAll('button')).find(b=>b.textContent==='Configure').click()}`)
	page.MustElement("#editor [name=role_label]").MustSelectAllText().MustInput("")
	page.MustElement("#editor [name=description]").MustSelectAllText().MustInput("")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	for _, agent := range snapshot.Agents {
		if agent.ID == "source" {
			require.Equal(t, model.AgentLabels{}, agent.Labels)
		} else {
			require.Equal(t, "engineer", agent.Labels.Role)
		}
	}
}

func TestBrowserAgentLabelsRemainIndependentBetweenGroups(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	desired := model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}
	labels := model.AgentLabels{Role: "fallback", Groups: map[model.GroupID]model.AgentDisplayLabels{"first": {Role: "reviewer", Description: "First group"}, "second": {Role: "author", Description: "Second group"}}}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "shared", "name": "Shared", "desired": desired, "labels": labels}, nil))
	for _, id := range []string{"first", "second"} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": id, "name": id, "members": []string{"shared"}}, nil))
	}
	page.MustElement("#refresh").MustClick()
	page.MustElementR(`[data-group-id="first"] .row`, "reviewer")
	page.MustElementR(`[data-group-id="second"] .row`, "author")
	page.MustElementR(`[data-group-id="first"] button`, "^Configure$").MustClick()
	require.Equal(t, "reviewer", page.MustElement("#editor [name=role_label]").MustProperty("value").Str())
	page.MustElement("#editor [name=role_label]").MustSelectAllText().MustInput("")
	page.MustElement("#editor [name=description]").MustSelectAllText().MustInput("")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustReload()
	page.MustElementR("#connection", "^Updated ")
	page.MustElementR(`[data-group-id="second"] .row`, "author")
	var snapshot struct {
		Agents []model.Agent `json:"agents"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 1)
	require.Equal(t, model.AgentDisplayLabels{}, snapshot.Agents[0].Labels.InGroup("first"))
	require.Equal(t, labels.Groups["second"], snapshot.Agents[0].Labels.InGroup("second"))
	require.Equal(t, "fallback", snapshot.Agents[0].Labels.Role)
}
