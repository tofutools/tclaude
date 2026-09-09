package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func TestInstalledCodexApprovalCompatibility(t *testing.T) {
	executable, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("Codex CLI unavailable")
	}
	p, err := New(Config{Executable: executable, PrivateRoot: t.TempDir()})
	require.NoError(t, err)
	policy := p.Capabilities().LaunchPolicy
	for _, mode := range []model.ApprovalMode{model.ApprovalOnRequest, model.ApprovalNever, model.ApprovalOnFailure, model.ApprovalUntrusted} {
		accepted := acceptsApproval(executable, mode)
		if accepted {
			require.Contains(t, policy.SupportedApproval, mode)
		} else {
			require.NotContains(t, policy.SupportedApproval, mode)
			_, err = p.Prepare(context.Background(), ports.PreparationRequest{Intent: ports.StartFresh, Spec: model.ResolvedExecutionSpec{Harness: Name, WorkingDirectory: t.TempDir(), Approval: mode, Sandbox: model.SandboxReadOnly}})
			require.ErrorContains(t, err, "approval mode")
		}
	}
}

func TestRemovedCodexApprovalRefusedBeforePreparation(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "codex")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\ncase \"$2\" in on-failure|untrusted) exit 2;; esac\nexit 0\n"), 0700))
	p, err := New(Config{Executable: executable, PrivateRoot: filepath.Join(root, "private")})
	require.NoError(t, err)
	for _, mode := range []model.ApprovalMode{model.ApprovalOnFailure, model.ApprovalUntrusted} {
		require.NotContains(t, p.Capabilities().LaunchPolicy.SupportedApproval, mode)
		_, err = p.Prepare(context.Background(), ports.PreparationRequest{Intent: ports.StartFresh, Spec: model.ResolvedExecutionSpec{Harness: Name, WorkingDirectory: t.TempDir(), Approval: mode, Sandbox: model.SandboxReadOnly}})
		require.ErrorContains(t, err, "approval mode")
	}
	_, err = os.Stat(filepath.Join(root, "private"))
	require.True(t, os.IsNotExist(err))
}
