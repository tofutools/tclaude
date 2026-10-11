package agentd

import (
	"context"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestArrivalGitProbeKeepsFailedFactsUnknown(t *testing.T) {
	cwd := testutil.CanonicalTempDir(t)
	require.NoError(t, exec.Command("git", "init", cwd).Run())
	_, ok, absent := arrivalGitProbe(context.Background(), cwd, "show-ref", "--quiet", "--verify", "--", "refs/heads/missing")
	require.False(t, ok)
	require.True(t, absent, "Git confirmed an absent ref")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, ok, absent = arrivalGitProbe(ctx, cwd, "show-ref", "--quiet", "--verify", "--", "refs/heads/missing")
	require.False(t, ok)
	require.False(t, absent, "an expired probe cannot establish absence")
	_, ok, absent = arrivalGitProbe(context.Background(), cwd+"/missing-dir", "show-ref", "--quiet", "--verify", "--", "refs/heads/missing")
	require.False(t, ok)
	require.False(t, absent, "an execution failure cannot establish absence")
}
