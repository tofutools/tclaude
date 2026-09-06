package product

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/migration"
	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
)

type inspectionFixture struct {
	bundle  migration.Bundle
	planned bool
	valid   bool
}

func (f *inspectionFixture) Inspect(_ context.Context, b migration.Bundle) (migration.Inspection, error) {
	f.bundle = b
	return migration.Inspection{Valid: f.valid, Snapshot: sourcev228.Snapshot{Config: []byte(`{"secret":"must-not-print"}`)}}, nil
}
func (f *inspectionFixture) Plan(i migration.Inspection) (migration.MigrationPlan, error) {
	f.planned = true
	return migration.MigrationPlan{PreflightValid: i.Valid, ExecutableConversion: false}, nil
}
func TestOfflineMigrationCommandsKeepPayloadPrivateAndReportBlockedStatus(t *testing.T) {
	for _, name := range []string{"inspect", "plan"} {
		for _, valid := range []bool{true, false} {
			fixture := &inspectionFixture{valid: valid}
			cmd := migrationCommand(fixture)
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs([]string{name, "--bundle", "/explicit/snapshot", "--manifest", "manifest.json"})
			err := cmd.Execute()
			if valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "blocking diagnostics")
			}
			require.Equal(t, migration.Bundle{Root: "/explicit/snapshot", ManifestPath: "manifest.json"}, fixture.bundle)
			require.Equal(t, name == "plan", fixture.planned)
			require.NotContains(t, output.String(), "must-not-print")
		}
	}
	cmd := migrationCommand(&inspectionFixture{})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"inspect"})
	require.Error(t, cmd.Execute())
}
