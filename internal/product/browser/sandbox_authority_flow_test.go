package browser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/client"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestBrowserSandboxAuthorityAllowsExactDelegatedLaunchAndRetainsEdits(t *testing.T) {
	p := &sandboxBrowserProvider{accessBrowserProvider: accessBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, delivered: make(chan ports.ActionCredentialReceipt, 4)}, requests: make(chan ports.PreparationRequest, 4)}
	ctx, page, operator := processEditorBrowser(t, p)
	var source app.SandboxProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/sandbox-profiles", map[string]any{"request_id": "source", "id": "source", "name": "Allowed boundary", "policy": model.SandboxPolicy{FilesystemRoot: model.SandboxRootSeparate}}, &source))
	var selected model.SandboxSelection
	require.NoError(t, operator.Call(ctx, "POST", "/v2/sandbox-profiles/selection", map[string]any{"scopes": []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: source.Revision.Ref}}}, &selected))
	desired := model.DesiredConfiguration{Harness: p.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	for _, id := range []string{"caller", "target", "without-policy"} {
		d := desired
		if id == "target" {
			d.HostSandbox = &selected
		}
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": id, "name": id, "desired": d}, nil))
	}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "team", "name": "Team", "members": []string{"caller", "target", "without-policy"}}, nil))
	require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "caller-start", "target": map[string]any{"agent": map[string]any{"agent_id": "caller", "expected_revision": 1}}}, nil))
	receipt, request := <-p.delivered, <-p.requests
	actor, err := client.New(request.AgentAPIEndpoint, receipt.Resource)
	require.NoError(t, err)
	defer actor.Close()
	launch := func(id, target string) error {
		return actor.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": id, "target": map[string]any{"agent": map[string]any{"agent_id": target, "expected_revision": 1}}}, nil)
	}
	var denied *client.Error
	require.ErrorAs(t, launch("before", "target"), &denied)
	require.Equal(t, 403, denied.Status)
	page.MustElement("#refresh").MustClick()
	page.MustElement("[data-tab=access]").MustClick()
	page.MustElement("#new-grant").MustClick()
	page.MustElement("#editor [name=subject]").MustSelect("Agent: caller · caller")
	page.MustElement("#editor [name=action]").MustSelect("execution.launch")
	page.MustElement("#editor [name=resource]").MustSelect("Current members of group: Team · team")
	page.MustElement("#editor [name=configuration]").MustSelect("Only the complete allow-lists below")
	page.MustElement("#editor [name=harnesses]").MustInput(p.Name())
	page.MustElement("#editor [name=models]").MustInput("fixture")
	page.MustElement("#editor [name=roots]").MustInput(desired.WorkingDirectory)
	page.MustElement("#editor [name=approvals]").MustSelect("supervised")
	page.MustElement("#editor [name=sandboxes]").MustSelect("workspace_write")
	page.MustElement("#editor [aria-label='Allowed sandbox profiles'] option[value=source]")
	page.MustElement("#editor [aria-label='Allowed sandbox profiles']").MustSelect("Allowed boundary")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`()=>!submitting`)
	page.MustElementR("#access-list button", "^Edit grant$").MustClick()
	page.MustElement("#editor").MustWaitVisible()
	page.MustElement("#editor [name=models]").MustSelectAllText().MustInput("fixture\nsecond")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`()=>!submitting`)
	var state app.AuthorityStateResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/authority", nil, &state))
	require.Len(t, state.Grants, 1)
	require.Equal(t, []string{string(source.Profile.ID)}, state.Grants[0].Bounds.HostSandboxProfiles)
	require.NoError(t, launch("allowed", "target"))
	require.ErrorAs(t, launch("wrong-policy", "without-policy"), &denied)
	require.Equal(t, 403, denied.Status)
}
