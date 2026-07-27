package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV165KeepsExistingOpenCodeRuntimesOnLoopback(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE schema_version SET version = 164`)
	require.NoError(t, err)
	for _, column := range []string{
		"transport", "control_socket_path", "control_socket_device", "control_socket_inode",
	} {
		_, err = d.Exec(`ALTER TABLE opencode_runtimes DROP COLUMN ` + column)
		require.NoError(t, err)
	}
	_, err = d.Exec(`
		INSERT INTO opencode_runtimes
			(session_id, server_url, password, pid, cwd, created_at, updated_at)
		VALUES ('legacy', 'http://127.0.0.1:44100', 'pw', 42, '/tmp', 'now', 'now')
	`)
	require.NoError(t, err)

	require.NoError(t, migrateV164toV165(d))
	runtime, err := GetOpenCodeRuntime("legacy")
	require.NoError(t, err)
	require.NotNil(t, runtime)
	assert.Equal(t, OpenCodeTransportLoopbackTCP, runtime.Transport)
	assert.Empty(t, runtime.ControlSocketPath)
	assert.Zero(t, runtime.ControlSocketDevice)
	assert.Zero(t, runtime.ControlSocketInode)
}
