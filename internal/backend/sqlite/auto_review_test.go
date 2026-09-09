package sqlite

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestAutoReviewBoundsPreserveInactiveNeverPolicy(t *testing.T) {
	for _, policy := range []model.ApprovalMode{model.ApprovalNever, model.ApprovalAutomatic, model.ApprovalOnRequest, model.ApprovalSupervised} {
		d := model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: t.TempDir(), Approval: policy, Sandbox: model.SandboxWorkspaceWrite, AutoReview: true}
		bounds := model.ConfigurationBounds{Harnesses: []string{d.Harness}, Models: []string{d.Model}, WorkingDirectoryRoots: []string{d.WorkingDirectory}, ApprovalModes: []model.ApprovalMode{policy}, SandboxModes: []model.SandboxMode{d.Sandbox}}
		require.Equal(t, policy == model.ApprovalNever || policy == model.ApprovalAutomatic, configurationMatches(bounds, &d))
		bounds.AutoReview = true
		require.True(t, configurationMatches(bounds, &d))
	}
}
