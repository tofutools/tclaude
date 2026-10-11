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

func TestBoardSkillGroupInstallAnchorsPreviewedProject(t *testing.T) {
	for _, mutation := range []string{"symlink", "replace-project", "none"} {
		t.Run(mutation, func(t *testing.T) {
			parent := testutil.CanonicalTempDir(t)
			base := filepath.Join(parent, "project")
			require.NoError(t, os.Mkdir(base, 0700))
			info, err := os.Stat(base)
			require.NoError(t, err)
			target := boardSkillRoot{ID: "group:test", Kind: "group", Path: filepath.Join(base, ".claude", "skills"), BoundaryIdentity: boardRootIdentity(info)}
			outside := testutil.CanonicalTempDir(t)
			switch mutation {
			case "symlink":
				require.NoError(t, os.Symlink(outside, filepath.Join(base, ".claude")))
			case "replace-project":
				require.NoError(t, os.Rename(base, base+"-old"))
				require.NoError(t, os.Mkdir(base, 0700))
			}
			incoming := skillbundle.Bundle{Format: skillbundle.Format, Name: "demo", Files: []skillbundle.File{{Path: "SKILL.md", Data: []byte("incoming")}}}
			err = installBoardSkill(incoming, []boardSkillRoot{target}, []configBundleChange{{Action: "create"}}, nil)
			if mutation == "none" {
				require.NoError(t, err)
				data, err := os.ReadFile(filepath.Join(target.Path, "demo", "SKILL.md"))
				require.NoError(t, err)
				require.Equal(t, "incoming", string(data))
			} else {
				require.Error(t, err)
				entries, err := os.ReadDir(outside)
				require.NoError(t, err)
				require.Empty(t, entries)
			}
		})
	}
}
