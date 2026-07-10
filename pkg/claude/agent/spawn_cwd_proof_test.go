package agent

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

func TestPrepareSpawnCwdWriteProof_ReportsSandboxWriteDenial(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(dir, []byte("blocked"), 0o600))
	proof := "signed-challenge"
	marker := filepath.Join(dir, clcommon.SpawnCwdProofPrefix+proof)

	prev := DaemonRequestImpl
	DaemonRequestImpl = func(method, path string, _ any, out any, _ DaemonOpts) error {
		require.Equal(t, http.MethodPost, method)
		require.Equal(t, "/v1/spawn-cwd-proof", path)
		resp := out.(*spawnCwdProofResponse)
		*resp = spawnCwdProofResponse{Required: true, Proof: proof, Cwd: dir, MarkerPath: marker}
		return nil
	}
	t.Cleanup(func() { DaemonRequestImpl = prev })

	gotProof, gotCwd, cleanup, err := prepareSpawnCwdWriteProof(dir)
	cleanup()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not writable by this agent's sandbox")
	assert.Empty(t, gotProof)
	assert.Empty(t, gotCwd)
}
