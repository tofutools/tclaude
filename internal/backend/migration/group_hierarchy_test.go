package migration

import (
	"context"
	"database/sql"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	db "github.com/tofutools/tclaude/internal/backend/sqlite"
	"os"
	"path/filepath"
	"testing"
)

func TestImportGroupHierarchyPreservesParentsAndRefusesCycles(t *testing.T) {
	for _, cycle := range []bool{false, true} {
		t.Run(map[bool]string{false: "nested", true: "cycle"}[cycle], func(t *testing.T) {
			bundle := buildFixture(t, fixtureOptions{})
			alterFixture(t, bundle, `ALTER TABLE agent_groups ADD COLUMN parent_id TEXT; INSERT INTO agent_groups(id,name) VALUES('2','parent');UPDATE agent_groups SET parent_id='2' WHERE id='1';`)
			if cycle {
				alterFixture(t, bundle, `UPDATE agent_groups SET parent_id='1' WHERE id='2';`)
			}
			path := filepath.Join(t.TempDir(), "import.db")
			result, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: path})
			if cycle {
				require.Error(t, err)
				_, statErr := os.Stat(path)
				require.ErrorIs(t, statErr, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			require.NotEmpty(t, result.Receipt.ID)
			inspection, err := Inspect(context.Background(), bundle)
			require.NoError(t, err)
			plan, err := Plan(inspection)
			require.NoError(t, err)
			store, err := db.Open(path)
			require.NoError(t, err)
			child, err := store.Group(context.Background(), model.GroupID(findIdentity(t, plan, "agent_groups", "1").TargetID))
			require.NoError(t, err)
			require.Equal(t, model.GroupID(findIdentity(t, plan, "agent_groups", "2").TargetID), child.ParentGroupID)
			require.NoError(t, store.Close())
			repeat, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: path})
			require.NoError(t, err)
			require.True(t, repeat.Repeated)
			raw, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			_, err = raw.Exec(`DELETE FROM group_parents`)
			require.NoError(t, err)
			require.NoError(t, raw.Close())
			_, err = ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: path})
			require.Error(t, err)
		})
	}
}
