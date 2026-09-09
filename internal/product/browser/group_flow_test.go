package browser

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/client"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"path/filepath"
	"testing"
)

func TestBrowserEditsGroupMembershipOrderAndBoundedOwner(t *testing.T) {
	p := &groupOwnerProvider{accessBrowserProvider: accessBrowserProvider{delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, delivered: make(chan ports.ActionCredentialReceipt, 2)}, endpoints: make(chan string, 2)}
	ctx, page, operator := processEditorBrowser(t, p)
	desired := model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	for _, id := range []string{"alpha", "beta", "gamma"} {
		require.NoError(t, operator.Call(ctx, "POST", "/v2/agents", map[string]any{"id": id, "name": id, "desired": func() model.DesiredConfiguration {
			d := desired
			if id == "alpha" {
				d.Harness = p.Name()
			}
			return d
		}()}, nil))
	}
	require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "team", "name": "Team", "members": []string{"alpha", "beta"}}, nil))
	page.MustElement("#refresh").MustClick()
	page.MustWait(`() => snapshot.groups?.some(group => group.ID === "team")`)
	page.MustElementR("summary", "^Group settings$").MustClick()
	page.MustElementR("#group-management h3", "Team")
	page.MustElementR("#group-management button", "^Edit name and members$").MustClick()
	page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("Updated team")
	page.MustElement("#editor [name=members]").MustSelect("gamma")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`() => !submitting`)
	page.MustElementR("#group-management button", "^Change owner$").MustClick()
	page.MustElement("#editor [name=owners]").MustSelect("alpha", "beta")
	page.MustElement("#editor [name=configuration]").MustSelect("Allow only the complete lists below")
	page.MustElement("#editor [name=harnesses]").MustInput("codex")
	page.MustElement("#editor [name=roots]").MustInput("/tmp")
	page.MustElement("#editor [name=approvals]").MustSelect("supervised")
	page.MustElement("#editor [name=sandboxes]").MustSelect("workspace_write")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElementR("#editor-error", "Supply all five allow-lists")
	page.MustElement("#editor [name=models]").MustInput("fixture")
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`() => !submitting`)
	var authority app.AuthorityStateResult
	require.NoError(t, operator.Call(ctx, "GET", "/v2/authority", nil, &authority))
	require.Len(t, authority.Assignments, 2)
	require.Equal(t, []string{"codex"}, authority.Assignments[0].Bounds.Harnesses)
	page.MustElementR("#group-management button", "^Change owner$").MustClick()
	require.Equal(t, "codex", page.MustElement("#editor [name=harnesses]").MustProperty("value").Str())
	page.MustElement("#editor button[type=submit]").MustClick()
	page.MustElement("#editor").MustWaitInvisible()
	page.MustWait(`() => !submitting`)
	page.MustElementR("#group-management .row button", "^Move down$").MustClick()
	var snapshot struct {
		Groups []model.Group `json:"groups"`
	}
	// Wait on the actual refreshed roster before reading the durable ordering.
	page.MustWait(`() => document.querySelector('#group-management .row').textContent.startsWith('beta')`)
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Groups, 1)
	require.Equal(t, "Updated team", snapshot.Groups[0].Name)
	require.Equal(t, []model.AgentID{"beta", "alpha", "gamma"}, snapshot.Groups[0].Members)
	require.Equal(t, model.AgentID("alpha"), snapshot.Groups[0].OwnerAgentID)
	require.ElementsMatch(t, []model.AgentID{"alpha", "beta"}, snapshot.Groups[0].OwnerAgentIDs)

	require.NoError(t, operator.Call(ctx, "POST", "/v2/launch", map[string]any{"request_id": "owner_start", "target": map[string]any{"agent": map[string]any{"agent_id": "alpha", "expected_revision": 1}}}, nil))
	receipt := <-p.delivered
	actor, err := client.New(<-p.endpoints, receipt.Resource)
	require.NoError(t, err)
	defer actor.Close()
	require.NoError(t, actor.Call(ctx, "PUT", "/v2/agents/beta", map[string]any{"name": "beta configured", "desired": desired, "expected_revision": 1}, nil))
	outside := desired
	outside.Model = "outside"
	err = actor.Call(ctx, "PUT", "/v2/agents/beta", map[string]any{"name": "beta outside", "desired": outside, "expected_revision": 2}, nil)
	var failure *client.Error
	require.ErrorAs(t, err, &failure)
	require.Equal(t, 403, failure.Status)
	// The owner form's shared limits also authorize adding a new offline member,
	// without a separately authored direct grant.
	var profile app.ConfigurationProfileResult
	require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "profile", "id": "worker", "revision_id": "one", "name": "Worker", "desired": desired}, &profile))
	require.NoError(t, operator.Call(ctx, "PUT", "/v2/groups/team/configuration", map[string]any{"profile": profile.Revision.Ref}, nil))
	var defaults model.GroupConfiguration
	require.NoError(t, actor.Call(ctx, "GET", "/v2/groups/team/configuration", nil, &defaults))
	var created app.GroupMemberResult
	require.NoError(t, actor.Call(ctx, "POST", "/v2/groups/team/agents", map[string]any{"request_id": "owner_create", "id": "new_member", "name": "New member", "expected_group_revision": snapshot.Groups[0].Revision, "expected_default_revision": defaults.Revision}, &created))
	require.Contains(t, created.Group.Members, model.AgentID("new_member"))
	require.Empty(t, created.Agent.PrimaryExecutionID)
}

type groupOwnerProvider struct {
	accessBrowserProvider
	endpoints chan string
}

func (p *groupOwnerProvider) Prepare(ctx context.Context, req ports.PreparationRequest) (ports.PreparedAttempt, error) {
	p.endpoints <- req.AgentAPIEndpoint
	return p.accessBrowserProvider.Prepare(ctx, req)
}
