package agent

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallCodexSkillsInstallsBothUserRoots(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, "custom-codex")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", codexHome)

	installed, err := InstallCodexSkills(true)

	require.NoError(t, err)
	assert.Len(t, installed, len(bundledSkills)*2)
	assert.DirExists(t, filepath.Join(home, ".agents", "skills", "agent-coord"))
	assert.DirExists(t, filepath.Join(codexHome, "skills", "agent-coord"))
}
