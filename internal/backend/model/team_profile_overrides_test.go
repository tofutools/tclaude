package model_test

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestTeamProfileOverridesKeepUnselectedFieldsAndResetForeignModel(t *testing.T) {
	base := model.DesiredConfiguration{Harness: "claude", Model: "sonnet", Effort: "high", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	other, nativeDefault := "codex", ""
	selection := &model.TeamProfileOverrides{Harness: &other, Effort: &nativeDefault}
	result := selection.Apply(base)
	require.Equal(t, "codex", result.Harness)
	require.Empty(t, result.Model)
	require.Empty(t, result.Effort)
	require.Equal(t, base.Approval, result.Approval)
	require.Equal(t, base.Sandbox, result.Sandbox)
	require.Equal(t, "sonnet", base.Model)
	chosen := "custom-model"
	selection.Model = &chosen
	require.Equal(t, chosen, selection.Apply(base).Model)
	require.Equal(t, base, (*model.TeamProfileOverrides)(nil).Apply(base))
}
