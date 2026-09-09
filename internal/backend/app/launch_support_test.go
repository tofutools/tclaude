package app_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/providers/claude"
	"github.com/tofutools/tclaude/internal/backend/providers/codex"
	"github.com/tofutools/tclaude/internal/backend/providers/copilot"
	"github.com/tofutools/tclaude/internal/backend/providers/opencode"
)

func TestLaunchSupportReadsAdapterContractWithoutPreparationOrStorage(t *testing.T) {
	// These uninitialized adapters and nil store cannot prepare native work. Their
	// static declarations are intentionally available without private host state.
	for _, tc := range []struct {
		provider  ports.Provider
		sandboxes []model.SandboxMode
	}{
		{&claude.Provider{}, []model.SandboxMode{model.SandboxWorkspaceWrite}},
		{&codex.Provider{}, []model.SandboxMode{model.SandboxReadOnly, model.SandboxWorkspaceWrite, model.SandboxUnconfined}},
		{&copilot.Provider{}, []model.SandboxMode{model.SandboxUnconfined}},
		{&opencode.Provider{}, []model.SandboxMode{model.SandboxUnconfined}},
	} {
		t.Run(tc.provider.Name(), func(t *testing.T) {
			service := app.New(nil, providers.NewRegistry(tc.provider))
			result, err := service.LaunchSupport(context.Background(), app.LaunchSupportRequest{Principal: model.OperatorPrincipal(), Harness: tc.provider.Name()})
			require.NoError(t, err)
			require.True(t, result.Configured)
			require.True(t, result.PolicyKnown)
			require.Equal(t, tc.sandboxes, result.SandboxModes)
			require.Contains(t, result.SandboxModes, result.DefaultSandbox)
			expectedApprovals := []model.ApprovalMode{model.ApprovalSupervised, model.ApprovalAutomatic}
			if tc.provider.Name() == opencode.Name {
				expectedApprovals = append(expectedApprovals, model.ApprovalDeny)
			}
			if tc.provider.Name() == codex.Name {
				expectedApprovals = append(expectedApprovals, model.ApprovalNever, model.ApprovalOnRequest, model.ApprovalOnFailure, model.ApprovalUntrusted)
			}
			if tc.provider.Name() == claude.Name {
				expectedApprovals = append(expectedApprovals, model.ApprovalInherit, model.ApprovalDefault, model.ApprovalManual, model.ApprovalPlan, model.ApprovalAcceptEdits, model.ApprovalAuto, model.ApprovalDontAsk, model.ApprovalBypassPermissions)
			}
			require.Equal(t, expectedApprovals, result.ApprovalModes)
			require.Contains(t, result.ApprovalModes, result.DefaultApproval)
			require.True(t, result.PreparedInitialInput)
			require.False(t, result.HostSandbox, "unconfigured adapters cannot prepare host sandboxes")
			result.SandboxModes[0] = "changed"
			require.Equal(t, tc.sandboxes, tc.provider.Capabilities().LaunchPolicy.SupportedSandbox)
			_, err = service.LaunchSupport(context.Background(), app.LaunchSupportRequest{Principal: model.AgentPrincipal("agent"), Harness: tc.provider.Name()})
			require.ErrorIs(t, err, app.ErrUnauthorized)
		})
	}
	service := app.New(nil, providers.NewRegistry(unknownPolicyProvider{}))
	missing, err := service.LaunchSupport(context.Background(), app.LaunchSupportRequest{Principal: model.OperatorPrincipal(), Harness: "missing"})
	require.NoError(t, err)
	require.False(t, missing.Configured)
	require.False(t, missing.PolicyKnown)
	unknown, err := service.LaunchSupport(context.Background(), app.LaunchSupportRequest{Principal: model.OperatorPrincipal(), Harness: "unknown"})
	require.NoError(t, err)
	require.True(t, unknown.Configured)
	require.False(t, unknown.PolicyKnown)
	_, err = service.LaunchSupport(context.Background(), app.LaunchSupportRequest{Principal: model.OperatorPrincipal()})
	require.ErrorIs(t, err, app.ErrInvalid)
}

type unknownPolicyProvider struct{ ports.Provider }

func (unknownPolicyProvider) Name() string { return "unknown" }
func (unknownPolicyProvider) Capabilities() ports.ProviderCapabilities {
	return ports.ProviderCapabilities{}
}
