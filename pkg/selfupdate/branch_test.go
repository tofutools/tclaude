package selfupdate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, msg string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte(msg), 0o644))
	gitIn(t, dir, "add", "f")
	gitIn(t, dir, "commit", "-q", "-m", msg)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// branchService is an updater whose fake build stamps each binary with the
// checkout's HEAD, as a real build stamps vcs.revision.
func branchService(t *testing.T, root, checkout, rev string) (*Service, string) {
	t.Helper()
	bin := filepath.Join(root, "gobin", "tclaude")
	if _, err := os.Stat(bin); err != nil {
		writeBin(t, bin, rev)
	}
	s, err := New(filepath.Join(root, "updates"), []Binary{{Name: "tclaude", Path: bin, Version: "v0.9.1-0.20261109000000-" + shortRev(rev), Method: "source", Revision: rev}}, Hooks{})
	require.NoError(t, err)
	s.SetFakesForTest(TestFakes{
		Stage: func(_ context.Context, _ Release, name, dir string) (string, error) {
			p := filepath.Join(dir, name)
			return p, os.WriteFile(p, []byte(gitIn(t, checkout, "rev-parse", "HEAD")), 0o700)
		},
		Inspect: func(context.Context, string) (buildversion.Info, error) {
			return buildversion.Info{Version: "v0.9.1-0.20261109000001-abc", ProtocolVersion: s.status.ProtocolVersion}, nil
		},
		Revision: func(path string) (string, bool, error) {
			raw, err := os.ReadFile(path)
			return string(raw), false, err
		},
	})
	return s, bin
}

func TestSourceCheckoutFollowsBranchFastForwardOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := testutil.CanonicalTempDir(t)
	origin, work, checkout := filepath.Join(root, "origin.git"), filepath.Join(root, "work"), filepath.Join(root, "checkout")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", origin)
	gitIn(t, root, "clone", "-q", origin, work)
	built := commit(t, work, "first")
	gitIn(t, work, "push", "-q", "origin", "HEAD:main")
	gitIn(t, root, "clone", "-q", origin, checkout)
	newer := commit(t, work, "fix the thing")
	gitIn(t, work, "push", "-q", "origin", "HEAD:main")

	s, bin := branchService(t, root, checkout, built)
	allow := func() bool { return true }
	opts := ApplyOptions{Method: MethodSourceCheckout, Checkout: CheckoutOptions{Path: checkout, Ref: "main"}}

	// A check compares origin/main with the running build's commit.
	job, err := s.StartWith(Request{Action: "check"}, "operator", allow, opts)
	require.NoError(t, err)
	job = waitJob(t, s, job.ID)
	require.Equal(t, "succeeded", job.State, job.Error)
	require.True(t, *job.UpdateAvailable)
	require.Equal(t, newer, job.Revision)
	require.Equal(t, "main@"+shortRev(newer), job.Version)
	require.Len(t, job.Commits, 1)
	require.Contains(t, job.Commits[0], "fix the thing")
	require.Equal(t, job.Commits, s.Status().PendingCommits)
	require.True(t, *s.Status().UpdateAvailable, "the status agrees with the branch check")

	// Uncommitted changes and local commits ahead are refused, untouched.
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "scratch"), []byte("wip"), 0o644))
	job, err = s.StartWith(Request{Action: "apply"}, "operator", allow, opts)
	require.NoError(t, err)
	require.Contains(t, waitJob(t, s, job.ID).Error, "uncommitted changes")
	require.NoError(t, os.Remove(filepath.Join(checkout, "scratch")))
	gitIn(t, checkout, "checkout", "-q", "-b", "feature")
	job, err = s.StartWith(Request{Action: "apply"}, "operator", allow, opts)
	require.NoError(t, err)
	require.Contains(t, waitJob(t, s, job.ID).Error, "does not switch branches")
	require.Equal(t, "feature", gitIn(t, checkout, "symbolic-ref", "--short", "HEAD"), "the operator's branch is left alone")
	gitIn(t, checkout, "checkout", "-q", "main")
	gitIn(t, checkout, "branch", "-q", "-D", "feature")
	local := commit(t, checkout, "local only")
	job, err = s.StartWith(Request{Action: "apply"}, "operator", allow, opts)
	require.NoError(t, err)
	require.Contains(t, waitJob(t, s, job.ID).Error, "only fast-forwards")
	require.Equal(t, local, gitIn(t, checkout, "rev-parse", "HEAD"), "the local commit is kept")
	gitIn(t, checkout, "reset", "-q", "--hard", built)

	// Applying fast-forwards, builds and installs where the binary lives.
	job, err = s.StartWith(Request{Action: "apply"}, "operator", allow, opts)
	require.NoError(t, err)
	job = waitJob(t, s, job.ID)
	require.Equal(t, "restarting", job.State, job.Error)
	require.Equal(t, newer, gitIn(t, checkout, "rev-parse", "HEAD"))
	raw, err := os.ReadFile(bin)
	require.NoError(t, err)
	require.Equal(t, newer, string(raw))
	require.True(t, s.Status().RollbackAvailable)

	// After the restart the build is current.
	s2, _ := branchService(t, root, checkout, newer)
	require.Equal(t, "succeeded", s2.Status().Job.State, s2.Status().Job.Error)
	require.Empty(t, s2.Status().PendingCommits, "the installed commits are no longer pending")
	require.False(t, *s2.Status().UpdateAvailable)
	job, err = s2.StartWith(Request{Action: "apply"}, "operator", allow, opts)
	require.NoError(t, err)
	job = waitJob(t, s2, job.ID)
	require.Equal(t, "succeeded", job.State, job.Error)
	require.False(t, *job.UpdateAvailable)
	require.False(t, job.RestartRequired, "nothing new, nothing rebuilt")
}

func TestSourceCheckoutNeverReplacesABuildAheadOfOrigin(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := testutil.CanonicalTempDir(t)
	origin, checkout := filepath.Join(root, "origin.git"), filepath.Join(root, "checkout")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", origin)
	gitIn(t, root, "clone", "-q", origin, checkout)
	commit(t, checkout, "first")
	gitIn(t, checkout, "push", "-q", "origin", "HEAD:main")
	// The running build has an unpushed fix on top of origin/main.
	unpushed := commit(t, checkout, "unpushed fix")
	s, _ := branchService(t, root, checkout, unpushed)
	job, err := s.StartWith(Request{Action: "apply"}, "scheduler", func() bool { return true }, ApplyOptions{Method: MethodSourceCheckout, Checkout: CheckoutOptions{Path: checkout, Ref: "main"}})
	require.NoError(t, err)
	job = waitJob(t, s, job.ID)
	require.Equal(t, "succeeded", job.State, job.Error)
	require.False(t, *job.UpdateAvailable, "an older origin commit is not an update")
	require.False(t, job.RestartRequired)
	require.Contains(t, strings.Join(job.Warnings, "\n"), "ahead or diverged")
}
