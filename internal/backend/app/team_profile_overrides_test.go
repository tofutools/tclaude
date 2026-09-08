//go:build linux || darwin

package app_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/providers/copilot"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

type teamCopilotFixture struct{ preparedWorkProvider }

func (*teamCopilotFixture) Name() string { return copilot.Name }
func (*teamCopilotFixture) Capabilities() ports.ProviderCapabilities {
	return (&copilot.Provider{}).Capabilities()
}

func TestTeamHarnessOverrideResolvesInheritedPolicyBeforeAdmission(t *testing.T) {
	for _, deny := range []bool{false, true} {
		for _, explicit := range []bool{false, true} {
			t.Run(map[bool]string{false: "confinement/", true: "approval/"}[deny]+map[bool]string{false: "inherited", true: "explicit"}[explicit], func(t *testing.T) {
				ctx := context.Background()
				store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
				require.NoError(t, err)
				defer store.Close()
				service := app.New(store, providers.NewRegistry(&teamCopilotFixture{}))
				op := model.OperatorPrincipal()
				base := model.DesiredConfiguration{Harness: "claude", Model: "sonnet", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
				if deny {
					base.Harness = "opencode"
					base.Approval = model.ApprovalDeny
					base.Sandbox = model.SandboxUnconfined
				}
				_, err = service.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "profile"}, ID: "profile", RevisionID: "one", Name: "Claude profile", Desired: base})
				require.NoError(t, err)
				harness := "copilot"
				sandbox := model.SandboxWorkspaceWrite
				overrides := &model.TeamProfileOverrides{Harness: &harness}
				if explicit {
					if deny {
						overrides.Approval = &base.Approval
					} else {
						overrides.Sandbox = &sandbox
					}
				}
				team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", ProfileID: "profile", Overrides: overrides}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"worker"}}}}
				saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: op, RequestID: "save"}, Draft: app.DefinitionDraft{ID: "team", RevisionID: "one", Name: "Team", Source: "fixture", Kind: model.DefinitionTeam, SchemaVersion: 1, Team: &team}})
				require.NoError(t, err)
				now := time.Now().UTC()
				require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: t.TempDir(), ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
				req := app.DeployTeamRequest{Context: app.RequestContext{Principal: op, RequestID: "deploy"}, DeploymentID: "deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionTeam}, GroupID: "group", Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "workspace", ExpectedRevision: 1}}}}
				result, err := service.DeployTeam(ctx, req)
				if explicit {
					require.ErrorIs(t, err, app.ErrInvalid)
					if deny {
						require.Contains(t, err.Error(), "approval deny is unsupported by copilot")
					} else {
						require.Contains(t, err.Error(), "confinement workspace_write is unsupported by copilot")
					}
					_, groupErr := store.Group(ctx, "group")
					require.ErrorIs(t, groupErr, app.ErrNotFound)
					return
				}
				require.NoError(t, err)
				agent, err := store.Agent(ctx, result.Deployment.Members["worker"])
				require.NoError(t, err)
				require.Equal(t, model.SandboxUnconfined, agent.Desired.Sandbox)
				if deny {
					require.Equal(t, model.ApprovalAutomatic, agent.Desired.Approval)
				} else {
					require.Equal(t, model.ApprovalSupervised, agent.Desired.Approval)
				}
				require.Empty(t, agent.Desired.Model)
			})
		}
	}
}
