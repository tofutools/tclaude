package browser

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"path/filepath"
	"testing"
)

type groupFallbackProvider struct{ automationTeamProvider }

func (p *groupFallbackProvider) Capabilities() ports.ProviderCapabilities {
	result := p.automationTeamProvider.Capabilities()
	result.LaunchPolicy = &ports.PolicyRequirements{DefaultApproval: model.ApprovalSupervised, DefaultSandbox: model.SandboxWorkspaceWrite, SupportedApproval: []model.ApprovalMode{model.ApprovalSupervised}, SupportedSandbox: []model.SandboxMode{model.SandboxWorkspaceWrite}}
	return result
}
func TestBrowserGroupMemberWithoutGroupProfileUsesDefaults(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(map[bool]string{false: "provider", true: "global"}[global], func(t *testing.T) {
			provider := &groupFallbackProvider{automationTeamProvider{name: "claude", delivery: host.ActionCredentialHost{PrivateRoot: filepath.Join(t.TempDir(), "credentials")}, briefs: make(chan string, 4)}}
			ctx, page, operator := processEditorBrowser(t, provider)
			cwd := t.TempDir()
			require.NoError(t, operator.Call(ctx, "POST", "/v2/groups", map[string]any{"id": "team", "name": "Team"}, nil))
			if global {
				var saved app.ConfigurationProfileResult
				desired := model.DesiredConfiguration{Harness: provider.Name(), Model: "fixture", WorkingDirectory: cwd, Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
				require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-profiles", map[string]any{"request_id": "profile", "id": "worker", "revision_id": "one", "name": "Worker", "desired": desired, "startup": model.ProfileStartup{AgentName: "Suggested", Role: "reviewer"}}, &saved))
				require.NoError(t, operator.Call(ctx, "POST", "/v2/configuration-defaults", map[string]any{"request_id": "global", "global": saved.Revision.Ref}, nil))
			}
			page.MustElement("#refresh").MustClick()
			page.MustWait(`()=>snapshot.groups?.some(g=>g.ID==='team')`)
			page.MustElementR("summary", "^Group settings$").MustClick()
			action := "^Create member$"
			if global {
				action = "^Spawn member$"
			}
			page.MustElementR("#group-management button", action).MustClick()
			if global {
				page.MustElement("#editor [name=brief]").MustInput("Use the global defaults")
				require.Equal(t, "Suggested", page.MustElement("#editor [name=name]").MustProperty("value").Str())
				require.Equal(t, "reviewer", page.MustElement("#editor [name=role_label]").MustProperty("value").Str())
			} else {
				page.MustElement("#editor [name=name]").MustSelectAllText().MustInput("New member")
				page.MustElement("#editor [name=harness]").MustSelect(provider.Name())
				page.MustElement("#editor [name=cwd]").MustInput(cwd)
			}
			page.MustElement("#editor button[type=submit]").MustClick()
			page.MustElement("#editor").MustWaitInvisible()
			page.MustWait(`()=>!submitting&&snapshot.agents?.length===1`)
			require.True(t, page.MustEval(`()=>snapshot.agents[0].Desired.Harness==='claude'&&snapshot.groups.find(g=>g.ID==='team').Members.includes(snapshot.agents[0].ID)`).Bool())
			var defaults model.GroupConfiguration
			require.NoError(t, operator.Call(ctx, "GET", "/v2/groups/team/configuration", nil, &defaults))
			if global {
				require.True(t, page.MustEval(`()=>Boolean(snapshot.agents[0].PrimaryExecutionID)`).Bool())
			}
			require.Nil(t, defaults.Profile)
			require.Zero(t, defaults.Revision)
		})
	}
}
