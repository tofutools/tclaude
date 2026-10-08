package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIdentityRegistryContainsEveryPeerColumn(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	rows, err := d.Query(`SELECT m.name,p.name FROM sqlite_master m JOIN pragma_table_info(m.name) p WHERE m.type='table'`)
	require.NoError(t, err)
	defer rows.Close()
	found := map[string]bool{}
	registered := map[string]bool{}
	for _, r := range FederationIdentityColumns {
		key := r.Table + "." + r.Column
		require.False(t, registered[key], "duplicate registry entry %s", key)
		registered[key] = true
		require.Contains(t, []string{IdentityRebind, IdentityHistorical, IdentityClose}, r.Rule)
	}
	for rows.Next() {
		var table, column string
		require.NoError(t, rows.Scan(&table, &column))
		key := table + "." + column
		found[key] = true
		if column == "peer" || column == "gateway" || column == "instance_id" || strings.HasSuffix(column, "_instance") || strings.HasSuffix(column, "_instance_id") || strings.HasSuffix(column, "_peer") || strings.HasSuffix(column, "_peer_id") {
			require.True(t, registered[key], "unclassified identity reference %s", key)
		}
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	foreign, err := d.Query(`SELECT m.name,f."from" FROM sqlite_master m JOIN pragma_foreign_key_list(m.name) f WHERE m.type='table' AND f."table"='federation_peers'`)
	require.NoError(t, err)
	for foreign.Next() {
		var table, column string
		require.NoError(t, foreign.Scan(&table, &column))
		require.True(t, registered[table+"."+column], "unclassified peer foreign key %s.%s", table, column)
	}
	require.NoError(t, foreign.Err())
	require.NoError(t, foreign.Close())
	for key := range registered {
		require.True(t, found[key], "registry references missing column %s", key)
	}
}
