package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/skillbundle"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestBoardSkillInstallRestoresDirectoryChangedAfterPreview(t *testing.T) {
	root := testutil.CanonicalTempDir(t)
	dir := filepath.Join(root, "demo")
	require.NoError(t, os.Mkdir(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("previewed"), 0600))
	old, e := skillbundle.Export(root, "demo")
	require.NoError(t, e)
	snapshot := boardPreviewHash(old)
	// Simulate an edit after preview/staging. The commit guard must inspect
	// the moved directory and restore this content rather than discard it.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("new local work"), 0600))
	incoming := skillbundle.Bundle{Format: skillbundle.Format, Name: "demo", Files: []skillbundle.File{{Path: "SKILL.md", Data: []byte("incoming")}}}
	err := installBoardSkill(incoming, []boardSkillRoot{{ID: "claude", Path: root}}, []configBundleChange{{Action: "replace"}}, map[string]string{"claude": snapshot})
	require.ErrorContains(t, err, "changed since preview")
	data, e := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	require.NoError(t, e)
	require.Equal(t, "new local work", string(data))
	entries, e := os.ReadDir(root)
	require.NoError(t, e)
	require.Len(t, entries, 1, "rollback removes private staging and restores the original directory")
}
