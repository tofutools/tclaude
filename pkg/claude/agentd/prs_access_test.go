package agentd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestPresentedPRAccessRequiresMatchingRepoInsideLaunchTree(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
		AllowedRemotes: []string{"github.com"},
	}}}))
	workspace := t.TempDir()
	repo := filepath.Join(workspace, "nested", "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	gitTestCommand(t, repo, "init")
	gitTestCommand(t, repo, "remote", "add", "origin", "git@github.com:tofutools/tclaude.git")

	const conv = "present-access-conv"
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: "present-access-session", ConvID: conv, Cwd: workspace,
	}))

	gotRoot, err := livePresentedPRAccessValidator(context.Background(), conv, repo,
		"https://github.com/tofutools/tclaude/pull/1")
	require.NoError(t, err)
	canonicalRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	require.Equal(t, canonicalRepo, gotRoot)
	_, err = livePresentedPRAccessValidator(context.Background(), conv, repo,
		"https://github.com/victim/private/pull/1")
	require.ErrorContains(t, err, "does not match repository origin")

	outside := t.TempDir()
	_, err = livePresentedPRAccessValidator(context.Background(), conv, outside,
		"https://github.com/tofutools/tclaude/pull/1")
	require.ErrorContains(t, err, "launch directory or a subdirectory")
}

func gitTestCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	require.NoError(t, cmd.Run())
}
