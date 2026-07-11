package db

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestMigrateV111toV112AddsSessionSandboxSnapshot(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	var have int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'effective_sandbox_config'`).Scan(&have))
	assert.Equal(t, 1, have)

	mustExec(t, d, `UPDATE schema_version SET version = 111`)
	require.NoError(t, migrateV111toV112(d))
	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 112, version)
}

func TestSessionSandboxSnapshotRoundTripAndHookStylePreservation(t *testing.T) {
	setupTestDB(t)
	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Effective.Environment = []sandboxpolicy.EnvironmentEntry{{Name: "LITERAL", Value: "value"}}
	require.NoError(t, SaveSession(&SessionRow{ID: "session-one", ConvID: "conv-one", EffectiveSandbox: &snapshot}))

	got, err := LoadSession("session-one")
	require.NoError(t, err)
	require.NotNil(t, got.EffectiveSandbox)
	assert.True(t, reflect.DeepEqual(snapshot, *got.EffectiveSandbox))

	// A later hook update does not carry launch-only policy; it must preserve
	// the existing immutable session-generation snapshot.
	got.EffectiveSandbox = nil
	got.Status = "working"
	require.NoError(t, SaveSession(got))
	preserved, err := LoadSession("session-one")
	require.NoError(t, err)
	require.NotNil(t, preserved.EffectiveSandbox)
	assert.True(t, reflect.DeepEqual(snapshot, *preserved.EffectiveSandbox))
}
