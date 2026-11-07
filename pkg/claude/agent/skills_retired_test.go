package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestInstallingTheDrawingBoardSkillRemovesTheRetiredAgentBoardSkill(t *testing.T) {
	root := testutil.CanonicalTempDir(t)
	old := filepath.Join(root, "agent-board")
	require.NoError(t, os.MkdirAll(old, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(old, "SKILL.md"), []byte("---\nname: agent-board\ndescription: x\n---\n"), 0o644))

	installed, err := installSkillsInRoot(root, false, []string{"agent-drawing-board"})
	require.NoError(t, err)
	require.Len(t, installed, 1)
	assert.FileExists(t, filepath.Join(root, "agent-drawing-board", "SKILL.md"))
	assert.NoDirExists(t, old, "the retired install is gone")
}

func TestARetiredSkillNameTheUserWroteStays(t *testing.T) {
	root := testutil.CanonicalTempDir(t)
	mine := filepath.Join(root, "agent-board")
	require.NoError(t, os.MkdirAll(mine, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mine, "SKILL.md"), []byte("---\nname: my-own-board\n---\n"), 0o644))

	_, err := installSkillsInRoot(root, false, []string{"agent-drawing-board"})
	require.NoError(t, err)
	assert.DirExists(t, mine, "a directory that is not the bundled skill is left alone")
}
