package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestRelaunchPolicyFromFlags(t *testing.T) {
	got, err := relaunchPolicyFromFlags(nil, "")
	require.NoError(t, err)
	assert.Nil(t, got, "neither flag: no rows")

	got, err = relaunchPolicyFromFlags([]string{"group_env=reuse", " harness_config = tighten-only "}, "")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"group_env": "reuse", "harness_config": "tighten-only"}, got)

	dir := testutil.CanonicalTempDir(t)
	file := filepath.Join(dir, "policy.json")
	require.NoError(t, os.WriteFile(file, []byte(`{"rows":[{"field":"group_env","mode":"reuse"},{"scope":"global","field":"sandbox_profile_contents","mode":"reuse"}]}`), 0o600))
	got, err = relaunchPolicyFromFlags([]string{"group_env=inherit"}, file)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"group_env": "inherit", "sandbox_profile_contents": "reuse"}, got, "a flag overrides the file's row")

	for _, bad := range [][]string{{"group_env"}, {"=reuse"}, {"group_env=chain-wins"}, {"harness=reuse"}} {
		_, err := relaunchPolicyFromFlags(bad, "")
		assert.Errorf(t, err, "%v", bad)
	}
	require.NoError(t, os.WriteFile(file, []byte(`{"rows":[{"scope":"group","field":"group_env","mode":"reuse"}]}`), 0o600))
	_, err = relaunchPolicyFromFlags(nil, file)
	assert.Error(t, err, "the file is checked with Validate")
}
