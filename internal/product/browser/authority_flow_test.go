package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/client"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"path/filepath"
	"testing"
)

func TestBrowserRoleAssignmentAndGrantScopeRemainExact(t *testing.T) {
	p := &groupOwnerProvider{accessBrowserProvider: accessBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, delivered: make(chan ports.ActionCredentialReceipt, 2)}, endpoints: make(chan string, 2)}
	ctx, page, operator := processEditorBrowser(t, p)
	desired := model.DesiredConfiguration{Harness: p.Name(), Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	for _, id := range []string{"alpha", "beta", "gamma"} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": id, "name": id, "desired": desired}, nil))
	}
	page.MustElement("#refresh").MustClick()
	page.MustElement("[data-tab=access]").MustClick()
	page.MustElementR("#access-list button", "^Create role$").MustClick()
	page.MustElement("#editor [name=name]").MustInput("Messenger")
	page.MustElementR("#editor button", "^Add permission$").MustClick()
	page.MustElement("#editor [aria-label='Permission action']").MustSelect("message.send")
	page.MustElementR("#editor summary", "^Named constraints$").MustClick()
	page.MustElement("#editor [aria-label='Target agents (one per line)']").MustInput("beta")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#access-list button", "^Assign role$").MustClick()
	page.MustElement("#editor [name=subject]").MustSelect("Agent: alpha · alpha")
	page.MustElement("#editor [name=resource]").MustSelect("Agent: beta · beta")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	var state app.AuthorityStateResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/authority", nil, &state))
	require.Len(t, state.Assignments, 1)
	require.Equal(t, model.AgentID("alpha"), state.Assignments[0].Subject.AgentID)
	require.Equal(t, model.AgentID("beta"), state.Assignments[0].Resource.AgentID)
	page.MustElementR("#access-list button", "^Edit role$").MustClick()
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Local edit")
	require.Equal(t, "beta", page.MustElement("#editor [aria-label='Target agents (one per line)']").MustProperty("value").Str())
	var role model.Role
	for _, candidate := range state.Roles {
		if candidate.Name == "Messenger" {
			role = candidate
		}
	}
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/authority/roles/"+string(role.ID), map[string]any{"name": "Concurrent edit", "actions": role.Actions, "scopes": role.Scopes, "expected_revision": role.Revision}, nil))
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElementR("#editor-error", "saved state changed")
	page.MustElement("#cancel").MustClick()
	page.MustElement("[data-tab=access]").MustClick()
	page.MustElementR("#access-list h3", "Concurrent edit")
	require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "start", "target": map[string]any{"agent": map[string]any{"agent_id": "alpha", "expected_revision": 1}}}, nil))
	receipt := <-p.delivered
	actor, err := client.New(<-p.endpoints, receipt.Resource)
	require.NoError(t, err)
	defer actor.Close()
	send := func(id, target string) error {
		return actor.Call(ctx, "POST", "/v2/messages", map[string]any{"request_id": id, "subject": "Scope", "body": "Hello", "to": model.MessageAudience{AgentIDs: []model.AgentID{model.AgentID(target)}}}, nil)
	}
	require.NoError(t, send("allowed", "beta"))
	var denied *client.Error
	require.ErrorAs(t, send("outside", "gamma"), &denied)
	require.Equal(t, 403, denied.Status)
	page.MustElementR("#access-list button", "^Remove assignment$").MustClick()
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.ErrorAs(t, send("revoked", "beta"), &denied)
	require.Equal(t, 403, denied.Status)
	page.MustElement("#new-grant").MustClick()
	page.MustElement("#editor [name=subject]").MustSelect("Agent: alpha · alpha")
	page.MustElement("#editor [name=action]").MustSelect("message.send")
	page.MustElement("#editor [name=resource]").MustSelect("Agent: beta · beta")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.NoError(t, send("direct", "beta"))
	page.MustElementR("#access-list button", "^Revoke grant$").MustClick()
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.ErrorAs(t, send("direct_revoked", "beta"), &denied)
	require.Equal(t, 403, denied.Status)
	page.MustElement("#new-grant").MustClick()
	page.MustElement("#editor [name=subject]").MustSelect("Agent: alpha · alpha")
	page.MustElement("#editor [name=action]").MustSelect("agent.configuration.update")
	page.MustElement("#editor [name=resource]").MustSelect("Agent: beta · beta")
	page.MustElement("#editor [name=configuration]").MustSelect("Only the complete allow-lists below")
	page.MustElement("#editor [name=harnesses]").MustInput(p.Name())
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElementR("#editor-error", "Supply all five allow-lists")
	page.MustElement("#editor [name=models]").MustInput("fixture")
	page.MustElement("#editor [name=roots]").MustInput("/tmp")
	page.MustElement("#editor [name=approvals]").MustSelect("supervised")
	page.MustElement("#editor [name=sandboxes]").MustSelect("workspace_write")
	page.MustElementR("#editor button", "^Add allowed environment$").MustClick()
	page.MustElementR("#editor button", "^Add variable$").MustClick()
	page.MustElement("#editor .launch-environment-row input").MustInput("APP_CHOICE")
	page.MustElement("#editor .launch-environment-row textarea").MustInput("allowed")
	desired.Environment = model.Environment{"APP_CHOICE": "allowed"}

	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.NoError(t, actor.Call(ctx, "PUT", "/v2/agents/beta", map[string]any{"name": "beta updated", "desired": desired, "expected_revision": 1}, nil))
	outside := desired
	outside.Model = "outside"
	require.ErrorAs(t, actor.Call(ctx, "PUT", "/v2/agents/beta", map[string]any{"name": "beta outside", "desired": outside, "expected_revision": 2}, nil), &denied)
	require.Equal(t, 403, denied.Status)

	outside = desired
	outside.Environment = model.Environment{"APP_CHOICE": "outside"}
	require.ErrorAs(t, actor.Call(ctx, "PUT", "/v2/agents/beta", map[string]any{"name": "beta outside environment", "desired": outside, "expected_revision": 2}, nil), &denied)
	require.Equal(t, 403, denied.Status)

}
