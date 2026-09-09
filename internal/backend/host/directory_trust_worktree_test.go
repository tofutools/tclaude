//go:build linux || darwin

package host

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDirectoryTrustRecognizesOnlyDefaultSiblingWorktree(t *testing.T) {
	repository := checkoutTestRepository(t)
	p := DirectoryProof{}
	ctx := context.Background()
	sibling := repository + "-worker"
	checkoutGit(t, repository, "worktree", "add", "--detach", sibling, "HEAD")
	dirs, err := p.DefaultSiblingTrustDirectories(ctx, sibling)
	require.NoError(t, err)
	physical, err := filepath.EvalSymlinks(sibling)
	require.NoError(t, err)
	require.Contains(t, dirs, physical)
	require.Len(t, dirs, 2, "proof includes the linked worktree's Git administration directory")
	git, err := NewCheckoutHost("")
	require.NoError(t, err)
	admin, err := git.gitOutput(ctx, sibling, "rev-parse", "--path-format=absolute", "--git-dir")
	require.NoError(t, err)
	admin, err = filepath.EvalSymlinks(admin)
	require.NoError(t, err)
	require.Contains(t, dirs, admin)
	other := filepath.Join(t.TempDir(), "elsewhere")
	checkoutGit(t, repository, "worktree", "add", "--detach", other, "HEAD")
	for _, path := range []string{repository, other, t.TempDir()} {
		dirs, err = p.DefaultSiblingTrustDirectories(ctx, path)
		require.NoError(t, err)
		require.Empty(t, dirs)
	}
}

func TestDirectoryTrustDoesNotRequireGitForOrdinaryNativeLaunch(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dirs, err := (DirectoryProof{}).DefaultSiblingTrustDirectories(context.Background(), t.TempDir())
	require.NoError(t, err)
	require.Empty(t, dirs)
}
