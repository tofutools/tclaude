package agentd_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestFlowDBTemplateCopiesIndependentNodes(t *testing.T) {
	for _, name := range []string{"first-node", "second-node"} {
		t.Run(name, func(t *testing.T) {
			f := newFlow(t)
			groups, err := db.ListAgentGroups()
			require.NoError(t, err)
			require.Empty(t, groups, "a prior node must not contaminate the template")
			f.HaveGroup(name)
			groups, err = db.ListAgentGroups()
			require.NoError(t, err)
			require.Len(t, groups, 1)
			require.Equal(t, name, groups[0].Name)
			backups, err := filepath.Glob(db.DBPath() + ".pre-*")
			require.NoError(t, err)
			require.Empty(t, backups, "a node copied from the current template must not replay migrations")
		})
	}
}
