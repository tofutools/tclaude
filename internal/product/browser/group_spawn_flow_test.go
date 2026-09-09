package browser

import (
	"context"
	"errors"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserGroupSpawnStartsOnceAfterLostReply(t *testing.T) {
	provider := &automationTeamProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, briefs: make(chan string, 4)}
	ctx, page, operator := processEditorBrowser(t, provider)
	desired := model.DesiredConfiguration{Harness: provider.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	var saved app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "profile", "id": "worker", "revision_id": "one", "name": "Worker", "desired": desired, "startup": model.ProfileStartup{AgentName: "Suggested", Context: "Reusable context", InitialMessage: "Suggested task"}}, &saved))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "team", "name": "Team"}, nil))
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/team/configuration", map[string]any{"profile": saved.Revision.Ref}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustWait(`()=>snapshot.groups?.some(g=>g.ID==='team')`)
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management button", "^Spawn member from default$").MustClick()
	require.Equal(t, "Suggested", page.MustElement("#editor [name=name]").MustProperty("value").Str())
	require.Equal(t, "Reusable context", page.MustElement("#editor [name=context]").MustProperty("value").Str())
	page.MustElement("#editor [name=brief]").MustSelectAllText().MustInput("Do this work")
	page.MustEval(`()=>{const original=fetch;window.spawnCalls=[];let lose=true;window.fetch=async(...args)=>{const response=await original(...args);if(String(args[0])==='/v2/groups/team/agents'){spawnCalls.push(JSON.parse(args[1].body));if(lose){lose=false;throw new Error('lost spawn reply')}}return response}}`)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElementR("#editor-error", "lost spawn reply")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`()=>!submitting`)
	require.True(t, page.MustEval(`()=>spawnCalls.length===2&&JSON.stringify(spawnCalls[0])===JSON.stringify(spawnCalls[1])`).Bool())
	select {
	case body := <-provider.briefs:
		require.Equal(t, "Reusable context\n\nDo this work", body)
	case <-time.After(time.Second):
		t.Fatal("missing prepared group spawn input")
	}
	select {
	case body := <-provider.briefs:
		t.Fatalf("duplicate launch: %s", body)
	default:
	}
	var snapshot struct {
		Agents     []model.Agent `json:"agents"`
		Executions []struct {
			ID string `json:"id"`
		} `json:"executions"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Agents, 1)
	require.Len(t, snapshot.Executions, 1)
	require.Equal(t, snapshot.Executions[0].ID, string(snapshot.Agents[0].PrimaryExecutionID))
	page.MustElementR("#roster button", "^Attach$")
}

// Only the native release boundary is uncertain; the public receipt, browser,
// application and durable member are real.
type uncertainGroupSpawnProvider struct{ *automationTeamProvider }

func (p *uncertainGroupSpawnProvider) Prepare(ctx context.Context, req ports.PreparationRequest) (ports.PreparedAttempt, error) {
	attempt, err := p.automationTeamProvider.Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	return &uncertainGroupSpawnAttempt{PreparedAttempt: attempt}, nil
}

type uncertainGroupSpawnAttempt struct{ ports.PreparedAttempt }

func (p *uncertainGroupSpawnAttempt) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	if err := permit.Consume(ctx); err != nil {
		return ports.ReleaseResult{}, err
	}
	return ports.ReleaseResult{State: ports.ReleaseUncertain, Evidence: p.Describe().Evidence}, errors.New("release response lost")
}
func TestBrowserGroupSpawnShowsUncertainReceiptOnFirstResponse(t *testing.T) {
	provider := &uncertainGroupSpawnProvider{&automationTeamProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, briefs: make(chan string, 4)}}
	ctx, page, operator := processEditorBrowser(t, provider)
	desired := model.DesiredConfiguration{Harness: provider.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	var saved app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "profile", "id": "worker", "revision_id": "one", "name": "Worker", "desired": desired}, &saved))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "team", "name": "Team"}, nil))
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/team/configuration", map[string]any{"profile": saved.Revision.Ref}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustWait(`()=>snapshot.groups?.some(g=>g.ID==='team')`)
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management button", "^Spawn member from default$").MustClick()
	page.MustElement("#editor [name=brief]").MustInput("Do the work")
	page.MustEval(`()=>{const original=fetch;window.spawnResponses=[];window.fetch=async(...args)=>{const response=await original(...args);if(String(args[0])==='/v2/groups/team/agents')spawnResponses.push(await response.clone().json());return response}}`)
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElementR("#editor-error", "launch is uncertain.*Inspect operation")
	require.True(t, page.MustEval(`()=>spawnResponses.length===1&&spawnResponses[0].Operation.state==='uncertain'&&snapshot.agents.length===1&&spawnResponses[0].Agent.PrimaryExecutionID===snapshot.agents[0].PrimaryExecutionID`).Bool())
	require.Contains(t, page.MustElement("#editor-error").MustText(), page.MustEval(`()=>spawnResponses[0].Operation.id`).Str())
	require.True(t, page.MustElement("#editor").MustVisible())
}
