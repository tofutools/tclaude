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

func TestImportGroupDetailsRoundTripAndTamperRefusal(t *testing.T) {
	ctx := context.Background()
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `ALTER TABLE agent_groups ADD COLUMN descr TEXT;ALTER TABLE agent_groups ADD COLUMN mission TEXT;ALTER TABLE agent_groups ADD COLUMN attachment_url TEXT;ALTER TABLE agent_groups ADD COLUMN attachment_label TEXT;UPDATE agent_groups SET descr='Description',mission='Mission',attachment_url='https://example.org/task',attachment_label='Task';`)
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
	require.Equal(t, &model.GroupDetails{Description: "Description", Mission: "Mission", LinkURL: "https://example.org/task", LinkLabel: "Task"}, group.Details)
	require.NoError(t, store.Close())
	repeated, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = raw.Exec(`DELETE FROM group_details`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	_, err = ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
	require.Error(t, err)
}

func TestImportUnsupportedGroupDetailsRemainDiagnosedEvidence(t *testing.T) {
	ctx := context.Background()
	bundle := buildFixture(t, fixtureOptions{})
	alterFixture(t, bundle, `ALTER TABLE agent_groups ADD COLUMN attachment_url TEXT;UPDATE agent_groups SET attachment_url='javascript:alert(1)';`)
	path := filepath.Join(t.TempDir(), "import.db")
	_, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
	require.NoError(t, err)
	report, err := ReadImportReport(ctx, path)
	require.NoError(t, err)
	found := false
	for _, d := range report.Diagnostics {
		if d.Code == "group_details_retained_unmapped" {
			found = true
		}
	}
	require.True(t, found)
	repeated, err := ImportSnapshot(ctx, bundle, ImportOptions{DestinationPath: path})
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
}
