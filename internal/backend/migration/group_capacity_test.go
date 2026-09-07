package migration

import (
	"context"
	"database/sql"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	db "github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
)

func TestImportGroupCapacityRoundTripAndTamperRefusal(t *testing.T) {
	ctx := context.Background()
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `ALTER TABLE agent_groups ADD COLUMN max_members INTEGER;UPDATE agent_groups SET max_members=2;`)
	path := filepath.Join(t.TempDir(), "import.db")
	_, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
	require.NoError(t, err)
	inspection, err := Inspect(ctx, bundle)
	require.NoError(t, err)
	plan, err := Plan(inspection)
	require.NoError(t, err)
	store, err := db.Open(path)
	require.NoError(t, err)
	group, err := store.Group(ctx, model.GroupID(findIdentity(t, plan, "agent_groups", "1").TargetID))
	require.NoError(t, err)
	require.Equal(t, int64(2), group.MaxActiveMembers)
	require.NoError(t, store.Close())
	repeated, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = raw.Exec(`UPDATE group_capacity SET max_active_members=3`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	_, err = ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
	require.Error(t, err)
}

func TestImportUnsupportedCapacityIsExplicitlyDiagnosed(t *testing.T) {
	ctx := context.Background()
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `ALTER TABLE agent_groups ADD COLUMN max_members INTEGER;UPDATE agent_groups SET max_members=-1;`)
	path := filepath.Join(t.TempDir(), "import.db")
	_, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
	require.NoError(t, err)
	report, err := ReadImportReport(ctx, path)
	require.NoError(t, err)
	found := false
	for _, d := range report.Diagnostics {
		if d.Code == "group_capacity_retained_unmapped" {
			found = true
		}
	}
	require.True(t, found)
}
