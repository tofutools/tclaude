package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenRetainsSnapshotReadPool(t *testing.T) {
	setupTestDB(t)
	database, err := Open()
	require.NoError(t, err)

	connections := make([]*sql.Conn, sqliteMaxIdleConnections)
	for i := range connections {
		connections[i], err = database.Conn(context.Background())
		require.NoError(t, err)
	}
	for _, connection := range connections {
		require.NoError(t, connection.Close())
	}

	assert.Equal(t, sqliteMaxIdleConnections, database.Stats().Idle,
		"all bounded snapshot-read connections stay warm between polls")
}
