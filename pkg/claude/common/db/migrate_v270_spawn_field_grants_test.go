package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A spawn grant's allowed_overrides list becomes a grant per field, with the
// spawn grant's scope (no expiry of their own), on peers and pools; the list
// is dropped.
func TestMigrateV270SpawnFieldGrants(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	require.NoError(t, TrustFederationPeer(FederationPeer{InstanceID: "inst_bobbobbob", PubKey: make([]byte, 32), Label: "bob"}))
	mustExec(t, d, `INSERT INTO federation_peer_grants(peer,slug,scope,spawn_policy,created_at,expires_at) VALUES('inst_bobbobbob','groups.members.spawn','group=1','{"max_live":2,"allowed_overrides":["model","effort","bogus"]}',1,99)`)
	mustExec(t, d, `INSERT INTO federation_peer_grants(peer,slug,scope,spawn_policy,created_at) VALUES('inst_bobbobbob','groups.members.spawn','group=2','{"max_live":3}',1)`)
	mustExec(t, d, `INSERT INTO federation_node_groups(id,name,created_at) VALUES('pool1','pool',1)`)
	mustExec(t, d, `INSERT INTO federation_node_group_grants(group_id,slug,scope,spawn_policy,created_at) VALUES('pool1','groups.members.spawn','','{"allowed_overrides":["cwd"]}',1)`)
	mustExec(t, d, `UPDATE schema_version SET version = 269`)
	require.NoError(t, migrateV269toV270(d))

	rows, err := d.Query(`SELECT slug,scope,spawn_policy,COALESCE(expires_at,0) FROM federation_peer_grants WHERE peer='inst_bobbobbob' ORDER BY slug,scope`)
	require.NoError(t, err)
	var got [][4]any
	for rows.Next() {
		var slug, scope, policy string
		var expires int64
		require.NoError(t, rows.Scan(&slug, &scope, &policy, &expires))
		got = append(got, [4]any{slug, scope, policy, expires})
	}
	require.NoError(t, rows.Close())
	require.Equal(t, [][4]any{
		{"groups.members.spawn", "group=1", `{"max_live":2}`, int64(99)},
		{"groups.members.spawn", "group=2", `{"max_live":3}`, int64(0)},
		{"groups.members.spawn.effort", "group=1", "{}", int64(0)},
		{"groups.members.spawn.model", "group=1", "{}", int64(0)},
	}, got)
	var pool string
	require.NoError(t, d.QueryRow(`SELECT spawn_policy FROM federation_node_group_grants WHERE group_id='pool1' AND slug='groups.members.spawn'`).Scan(&pool))
	require.Equal(t, "{}", pool)
	var n int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM federation_node_group_grants WHERE group_id='pool1' AND slug='groups.members.spawn.cwd'`).Scan(&n))
	require.Equal(t, 1, n)
}
