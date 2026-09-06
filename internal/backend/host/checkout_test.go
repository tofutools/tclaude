//go:build linux || darwin

package host

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckoutCreateInspectRestoreAndRemove(t *testing.T) {
	repository := checkoutTestRepository(t)
	host, err := NewCheckoutHost("git")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "worker")

	created, err := host.Create(context.Background(), CheckoutIntent{
		Repository: repository, Path: path, Branch: "feature/history", Base: "HEAD",
	})
	require.NoError(t, err)
	require.Equal(t, CheckoutEffectReady, created.State)
	require.Equal(t, CheckoutCreated, created.Evidence.Ownership)
	require.True(t, created.Evidence.Linked)
	require.NotEmpty(t, created.Evidence.OwnerToken)

	observed, err := host.Inspect(context.Background(), created.Evidence)
	require.NoError(t, err)
	require.Equal(t, CheckoutEffectReady, observed.State)
	require.Equal(t, created.Evidence.InitialCommit, observed.Commit)
	require.False(t, observed.Dirty)
	require.NoError(t, os.WriteFile(filepath.Join(path, "worker.txt"), []byte("committed worker result\n"), 0o600))
	checkoutGit(t, path, "add", "worker.txt")
	checkoutGit(t, path, "commit", "-m", "worker result")
	advancedCommit, err := host.gitOutput(context.Background(), path, "rev-parse", "HEAD")
	require.NoError(t, err)
	require.NotEqual(t, created.Evidence.InitialCommit, advancedCommit)

	require.NoError(t, os.RemoveAll(path))
	absent, err := host.Inspect(context.Background(), created.Evidence)
	require.NoError(t, err)
	require.Equal(t, CheckoutEffectAbsent, absent.State)

	restored, err := host.Restore(context.Background(), created.Evidence)
	require.NoError(t, err)
	require.Equal(t, CheckoutEffectReady, restored.State)
	require.Equal(t, created.Evidence.OwnerToken, restored.Evidence.OwnerToken)
	require.DirExists(t, path)
	restoredObservation, err := host.Inspect(context.Background(), restored.Evidence)
	require.NoError(t, err)
	require.Equal(t, advancedCommit, restoredObservation.Commit)

	removed, err := host.Remove(context.Background(), CheckoutRemovalRequest{Evidence: restored.Evidence})
	require.NoError(t, err)
	require.Equal(t, CheckoutEffectAbsent, removed.State)
	require.NoDirExists(t, path)
	// Checkout removal deliberately retains the branch.
	require.NoError(t, exec.Command("git", "-C", repository, "show-ref", "--verify", "--quiet", "refs/heads/feature/history").Run())
}

func TestCheckoutOrdinaryRemovalRefusesUseSharingAndDirtiness(t *testing.T) {
	repository := checkoutTestRepository(t)
	host, err := NewCheckoutHost("git")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "worker")
	created, err := host.Create(context.Background(), CheckoutIntent{
		Repository: repository, Path: path, Branch: "feature/refusals",
	})
	require.NoError(t, err)

	_, err = host.Remove(context.Background(), CheckoutRemovalRequest{Evidence: created.Evidence, UseCount: 1})
	require.ErrorIs(t, err, ErrCheckoutInUse)
	_, err = host.Remove(context.Background(), CheckoutRemovalRequest{Evidence: created.Evidence, Shared: true})
	require.ErrorIs(t, err, ErrCheckoutShared)
	require.NoError(t, os.WriteFile(filepath.Join(path, "untracked.txt"), []byte("retain me\n"), 0o600))
	_, err = host.Remove(context.Background(), CheckoutRemovalRequest{Evidence: created.Evidence})
	require.ErrorIs(t, err, ErrCheckoutDirty)
	require.FileExists(t, filepath.Join(path, "untracked.txt"))

	removed, err := host.Remove(context.Background(), CheckoutRemovalRequest{
		Evidence: created.Evidence, UseCount: 1, Shared: true, Destructive: true,
	})
	require.NoError(t, err)
	require.Equal(t, CheckoutEffectAbsent, removed.State)
}

func TestCheckoutRegistrationDoesNotManufactureOwnership(t *testing.T) {
	repository := checkoutTestRepository(t)
	host, err := NewCheckoutHost("git")
	require.NoError(t, err)

	registered, err := host.Register(context.Background(), repository)
	require.NoError(t, err)
	require.Equal(t, CheckoutRegistered, registered.Ownership)
	require.Empty(t, registered.OwnerToken)
	_, err = host.Remove(context.Background(), CheckoutRemovalRequest{Evidence: registered, Destructive: true})
	require.ErrorIs(t, err, ErrCheckoutNotOwned)
	require.DirExists(t, repository)

	adopted, err := host.Adopt(context.Background(), repository)
	require.NoError(t, err)
	_, err = host.Remove(context.Background(), CheckoutRemovalRequest{Evidence: adopted, Destructive: true})
	require.ErrorIs(t, err, ErrCheckoutMain)
	require.DirExists(t, repository)
}

func TestCheckoutRejectsFabricatedOwnershipEvidence(t *testing.T) {
	repository := checkoutTestRepository(t)
	host, err := NewCheckoutHost("git")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "worker")
	created, err := host.Create(context.Background(), CheckoutIntent{
		Repository: repository, Path: path, Branch: "feature/identity",
	})
	require.NoError(t, err)

	forged := created.Evidence
	forged.OwnerToken = "fabricated"
	_, err = host.Inspect(context.Background(), forged)
	require.True(t, errors.Is(err, ErrCheckoutIdentityMismatch))
	_, err = host.Remove(context.Background(), CheckoutRemovalRequest{Evidence: forged, Destructive: true})
	require.ErrorIs(t, err, ErrCheckoutIdentityMismatch)
	require.DirExists(t, path)
}

func TestCheckoutRestoreRefusalPreservesOwnershipEvidence(t *testing.T) {
	repository := checkoutTestRepository(t)
	host, err := NewCheckoutHost("git")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "worker")
	created, err := host.Create(context.Background(), CheckoutIntent{
		Repository: repository, Path: path, Branch: "feature/restore-refusal", Base: "HEAD",
	})
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(path))
	unrelated, err := host.gitOutput(context.Background(), repository, "commit-tree", "HEAD^{tree}", "-m", "unrelated")
	require.NoError(t, err)
	checkoutGit(t, repository, "update-ref", "refs/heads/feature/restore-refusal", unrelated)

	for range 2 {
		_, err = host.Restore(context.Background(), created.Evidence)
		require.ErrorIs(t, err, ErrCheckoutIdentityMismatch)
		require.NoError(t, verifyCheckoutOwner(created.Evidence.GitDir, created.Evidence.OwnerToken))
	}
}

func TestCheckoutRefusesExistingBranchAtDifferentSelectedBase(t *testing.T) {
	repository := checkoutTestRepository(t)
	checkoutGit(t, repository, "branch", "existing", "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(repository, "second.txt"), []byte("second\n"), 0o600))
	checkoutGit(t, repository, "add", "second.txt")
	checkoutGit(t, repository, "commit", "-m", "second")
	host, err := NewCheckoutHost("git")
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "worker")
	result, err := host.Create(context.Background(), CheckoutIntent{
		Repository: repository, Path: path, Branch: "existing", Base: "HEAD",
	})
	require.ErrorIs(t, err, ErrCheckoutBaseConflict)
	require.Empty(t, result.Evidence.Path)
	require.NoDirExists(t, path)
}

func checkoutTestRepository(t *testing.T) string {
	t.Helper()
	repository := t.TempDir()
	checkoutGit(t, repository, "init", "-b", "main")
	checkoutGit(t, repository, "config", "user.name", "Checkout Test")
	checkoutGit(t, repository, "config", "user.email", "checkout@example.invalid")
	require.NoError(t, os.WriteFile(filepath.Join(repository, "README.md"), []byte("initial\n"), 0o600))
	checkoutGit(t, repository, "add", "README.md")
	checkoutGit(t, repository, "commit", "-m", "initial")
	return repository
}

func checkoutGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", directory}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
}
