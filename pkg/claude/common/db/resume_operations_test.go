package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/platform/execution"
)

func setupResumeOperationSchema(t *testing.T) {
	t.Helper()
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE schema_version SET version = 227`)
	require.NoError(t, err)
	require.NoError(t, migrateV227toV228(d))
}

func TestMigrateV227toV228ResumeOperationsIsIdempotent(t *testing.T) {
	setupResumeOperationSchema(t)
	d, err := Open()
	require.NoError(t, err)
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 228, version)
	var tableCount int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='execution_operations'`).Scan(&tableCount))
	assert.Equal(t, 1, tableCount)
	var columnCount int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name='resume_operation_id'`).Scan(&columnCount))
	assert.Equal(t, 1, columnCount)
	require.NoError(t, migrateV227toV228(d))
}

func TestResumeOperationStoreTransitionsAndReconcilesUnknown(t *testing.T) {
	setupResumeOperationSchema(t)
	opID := execution.NewOperationID()
	eID := execution.ID("11111111111111111111111111111111")
	require.NoError(t, CreateResumeOperation(ResumeOperationRow{
		ID: opID, Kind: "manual_resume", ConvID: "resume-store-conv",
		Attempt: execution.AttemptRef{ExecutionID: eID, LegacySessionID: "session-new"},
		State:   execution.ResumeRequested, LaunchPhase: "requested", Revision: 1,
	}))
	require.NoError(t, TransitionResumeOperation(opID, 1, execution.ResumeAccepted, "accepted", ""))
	require.NoError(t, TransitionResumeOperation(opID, 2, execution.ResumeUnknown, "wrapper_ambiguous", "fork outcome unknown"))
	require.NoError(t, TransitionResumeOperation(opID, 3, execution.ResumeReady, "released", ""))
	row, err := GetResumeOperation(opID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, execution.ResumeReady, row.State)
	assert.Equal(t, int64(4), row.Revision)
	active, err := ActiveResumeOperations()
	require.NoError(t, err)
	assert.Empty(t, active)
}
