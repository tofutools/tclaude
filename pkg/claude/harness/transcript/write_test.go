package transcript

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArtifactWritesNeverFollowPlantedPaths(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "transcript.md")))
	assert.Error(t, WriteFileExclusive(filepath.Join(dir, "transcript.md"), []byte("x")))
	_, err := os.Stat(outside)
	assert.True(t, os.IsNotExist(err), "the planted symlink was not followed")

	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(dir, "media")))
	assert.Error(t, EnsureRealDir(filepath.Join(dir, "media")))
	require.NoError(t, EnsureRealDir(filepath.Join(dir, "fresh")))
	require.NoError(t, EnsureRealDir(filepath.Join(dir, "fresh")), "an existing real directory is accepted")
}
