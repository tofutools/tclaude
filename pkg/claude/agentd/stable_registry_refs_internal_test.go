package agentd

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestRoleProfileResolutionUsesIDLoadedBeforeRename(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	profileID, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "before"})
	require.NoError(t, err)
	_, err = db.CreateRole(&db.Role{Name: "stable-role-race", SpawnProfile: "before"})
	require.NoError(t, err)
	loadedBeforeRename, err := db.GetRole("stable-role-race")
	require.NoError(t, err)
	require.Equal(t, profileID, loadedBeforeRename.SpawnProfileID)

	require.NoError(t, db.UpdateSpawnProfile(&db.SpawnProfile{ID: profileID, Name: "after"}))
	_, fail := resolveTemplateAgentLaunch(db.GroupTemplateAgent{}, loadedBeforeRename, home, "")
	require.Nil(t, fail, "the pre-rename role object must resolve its profile through the stable id")
}
