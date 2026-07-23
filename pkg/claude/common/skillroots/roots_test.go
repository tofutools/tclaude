package skillroots

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllReturnsAbsoluteLiteralSkillRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", "")

	roots, err := All()

	require.NoError(t, err)
	assert.Equal(t, []string{
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(home, ".agents", "skills"),
		filepath.Join(home, ".codex", "skills"),
	}, roots)
}

func TestCodexDeduplicatesResolvedRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".agents"))

	roots, err := Codex()

	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join(home, ".agents", "skills")}, roots)
}
