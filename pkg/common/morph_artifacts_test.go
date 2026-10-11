package common

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMorphArtifactsDirIsPerAgentAndRefusesSymlinks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const agent = "agt_0123"
	assert.Empty(t, ExistingMorphArtifactsDir(agent), "nothing is granted before a morph")

	dir, err := PrepareMorphArtifactsDir(agent)
	require.NoError(t, err)
	assert.Equal(t, MorphArtifactsBase(), filepath.Dir(dir), "a direct child of the hidden parent")
	assert.Equal(t, dir, ExistingMorphArtifactsDir(agent))
	assert.NotEqual(t, dir, MorphArtifactsDir("agt_other"))
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	other := MorphArtifactsDir("agt_link")
	require.NoError(t, os.Symlink(t.TempDir(), other))
	_, err = PrepareMorphArtifactsDir("agt_link")
	assert.Error(t, err, "a planted symlink is never accepted as the agent's directory")
	assert.Empty(t, ExistingMorphArtifactsDir("agt_link"), "nor granted to a launch")
}
