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

// Native execution is simulated; application admission, credentials and public
// authoring/effect routes are real. Provider policy matrices have backend tests.
type atomicSpawnBrowserProvider struct{ groupOwnerProvider }

func (*atomicSpawnBrowserProvider) ApprovalPosture(d model.DesiredConfiguration) model.ApprovalPosture {
	return model.ApprovalPosture{Known: d.Approval == model.ApprovalSupervised && !d.AutoReview}
}
func (*atomicSpawnBrowserProvider) RequestedSandboxPosture(s model.ResolvedExecutionSpec) model.SandboxPosture {
	if s.Sandbox == model.SandboxWorkspaceWrite && s.HostSandbox == nil {
		return model.SandboxPostureCodexWorkspace
	}
	return model.SandboxPostureUnknown
}
func (p *atomicSpawnBrowserProvider) RecordedSandboxPosture(e model.Execution) model.SandboxPosture {
	return p.RequestedSandboxPosture(e.Spec)
}

func TestBrowserAtomicSpawnGrantDoesNotGrantExistingAgentLaunch(t *testing.T) {
	p := &atomicSpawnBrowserProvider{groupOwnerProvider{accessBrowserProvider: accessBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, delivered: make(chan ports.ActionCredentialReceipt, 4)}, endpoints: make(chan string, 4)}}
	ctx, page, operator := processEditorBrowser(t, p)
	desired := model.DesiredConfiguration{Harness: p.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": "lead", "name": "Lead", "desired": desired}, nil))
	var profile app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"id": "worker", "request_id": "profile", "revision_id": "one", "name": "Worker", "desired": desired}, &profile))
	for _, group := range []string{"team", "other"} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": group, "name": group}, nil))
		require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/"+group+"/configuration", map[string]any{"profile": profile.Revision.Ref}, nil))
	}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "start", "target": map[string]any{"agent": map[string]any{"agent_id": "lead", "expected_revision": 1}}}, nil))
	receipt := <-p.delivered
	actor, err := client.New(<-p.endpoints, receipt.Resource)
	require.NoError(t, err)
	defer actor.Close()
	spawn := func(group string) error {
		return actor.Call(ctx, "POST", "/v2/groups/"+group+"/agents", map[string]any{"request_id": "spawn", "id": "child", "name": "Child", "launch": map[string]any{}, "expected_group_revision": 1, "expected_default_revision": 1}, nil)
	}
	var denied *client.Error
	require.ErrorAs(t, spawn("team"), &denied)
	require.Equal(t, 403, denied.Status)
	page.MustElement("#refresh").MustClick()
	page.MustElementR("#connection", "^Updated")
	page.MustElement("[data-tab=access]").MustClick()
	page.MustElement("#new-grant").MustClick()
	page.MustElement("#editor [name=subject]").MustSelect("Agent: Lead · lead")
	page.MustElement("#editor [name=action]").MustSelect("group.members.spawn")
	page.MustElement("#editor [name=resource]").MustSelect("Group: team · team")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustElementR("#access-list p", "New-member spawn follows")
	require.NoError(t, actor.Call(ctx, "GET", "/v2/groups/team/configuration", nil, nil))
	require.NoError(t, spawn("team"))
	require.NoError(t, spawn("team"))
	require.ErrorAs(t, spawn("other"), &denied)
	require.Equal(t, 403, denied.Status)
	require.ErrorAs(t, actor.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "restart", "target": map[string]any{"agent": map[string]any{"agent_id": "child", "expected_revision": 1}}}, nil), &denied)
	require.Equal(t, 403, denied.Status)
	page.MustElementR("#access-list button", "^Revoke grant$").MustClick()
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	require.ErrorAs(t, spawn("team"), &denied)
	require.Equal(t, 403, denied.Status)
}
