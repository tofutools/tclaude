package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const FederationNodeGroupScopePrefix = "group-id:"

var ErrNodeGroupNotFound = errors.New("node group not found")
var ErrNodeGroupEmpty = errors.New("node group has no trusted members")

type FederationNodeGroup struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

func CreateFederationNodeGroup(name string) (*FederationNodeGroup, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	g := &FederationNodeGroup{ID: proto.NewEnvelopeID(), Name: name, CreatedAt: time.Now().UTC()}
	_, err = d.Exec(`INSERT INTO federation_node_groups(id,name,created_at) VALUES(?,?,?)`, g.ID, g.Name, dbTime(g.CreatedAt))
	return g, err
}
func GetFederationNodeGroup(name string) (*FederationNodeGroup, error) {
	return getFederationNodeGroup("name", name)
}
func GetFederationNodeGroupByID(id string) (*FederationNodeGroup, error) {
	return getFederationNodeGroup("id", id)
}
func getFederationNodeGroup(column, value string) (*FederationNodeGroup, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var g FederationNodeGroup
	var at dbTimestamp
	err = d.QueryRow(`SELECT id,name,created_at FROM federation_node_groups WHERE `+column+`=?`, value).Scan(&g.ID, &g.Name, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNodeGroupNotFound
	}
	g.CreatedAt = at.Time()
	return &g, err
}
func ListFederationNodeGroups() ([]FederationNodeGroup, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id,name,created_at FROM federation_node_groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FederationNodeGroup{}
	for rows.Next() {
		var g FederationNodeGroup
		var at dbTimestamp
		if err = rows.Scan(&g.ID, &g.Name, &at); err != nil {
			return nil, err
		}
		g.CreatedAt = at.Time()
		out = append(out, g)
	}
	return out, rows.Err()
}
func AddFederationNodeGroupPeer(id, peer string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO federation_node_group_members(group_id,peer) VALUES(?,?) ON CONFLICT DO NOTHING`, id, peer)
	return err
}
func RemoveFederationNodeGroupPeer(id, peer string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM federation_node_group_members WHERE group_id=? AND peer=?`, id, peer)
	return err
}
func DeleteFederationNodeGroup(id string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM federation_node_groups WHERE id=?`, id)
	return err
}

// Membership always joins current trust, including callers using an old pool ID.
func FederationNodeGroupContainsID(id, peer string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var n int
	err = d.QueryRow(`SELECT count(*) FROM federation_node_group_members m JOIN federation_peers p ON p.instance_id=m.peer JOIN federation_node_groups g ON g.id=m.group_id WHERE m.group_id=? AND m.peer=?`, id, peer).Scan(&n)
	return n > 0, err
}
func ListFederationNodeGroupMembers(id string) ([]FederationPeer, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT p.instance_id FROM federation_node_group_members m JOIN federation_peers p ON p.instance_id=m.peer WHERE m.group_id=? ORDER BY p.instance_id`, id)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var peer string
		if err = rows.Scan(&peer); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, peer)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	out := []FederationPeer{}
	for _, id := range ids {
		p, e := GetFederationPeer(id)
		if e != nil {
			return nil, e
		}
		if p != nil {
			out = append(out, *p)
		}
	}
	return out, nil
}

// Placement gets explicit errors, while grants use the fail-closed predicate.
func ListFederationNodeGroupPeers(name string) ([]FederationPeer, error) {
	g, err := GetFederationNodeGroup(name)
	if err != nil {
		return nil, err
	}
	peers, err := ListFederationNodeGroupMembers(g.ID)
	if err != nil {
		return nil, err
	}
	if len(peers) == 0 {
		return nil, ErrNodeGroupEmpty
	}
	return peers, nil
}
func UpsertFederationNodeGroupGrant(id string, g FederationPeerGrant) error {
	d, err := Open()
	if err != nil {
		return err
	}
	policy, err := json.Marshal(g.SpawnPolicy)
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO federation_node_group_grants(group_id,slug,scope,spawn_policy,created_at) VALUES(?,?,?,?,?) ON CONFLICT(group_id,slug,scope) DO UPDATE SET spawn_policy=excluded.spawn_policy`, id, g.Slug, g.Scope, string(policy), dbTime(time.Now()))
	return err
}
func DeleteFederationNodeGroupGrant(id, slug, scope string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	r, err := d.Exec(`DELETE FROM federation_node_group_grants WHERE group_id=? AND slug=? AND scope=?`, id, slug, scope)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}
func ListFederationNodeGroupGrants(id string) ([]FederationPeerGrant, error) {
	return listFederationPoolGrants(id, "")
}
func listFederationPoolGrants(id, peer string) ([]FederationPeerGrant, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT g.id,g.name,a.slug,a.scope,a.spawn_policy,a.created_at FROM federation_node_group_grants a JOIN federation_node_groups g ON g.id=a.group_id WHERE (?='' OR g.id=?) AND (?='' OR EXISTS(SELECT 1 FROM federation_node_group_members m JOIN federation_peers p ON p.instance_id=m.peer WHERE m.group_id=g.id AND m.peer=?)) ORDER BY g.name,a.slug,a.scope`, id, id, peer, peer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FederationPeerGrant{}
	for rows.Next() {
		var g FederationPeerGrant
		var raw string
		var at dbTimestamp
		if err = rows.Scan(&g.PoolID, &g.PoolName, &g.Slug, &g.Scope, &raw, &at); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &g.SpawnPolicy); err != nil {
			return nil, err
		}
		g.Peer = peer
		if peer == "" {
			g.Peer = "group:" + g.PoolName
		}
		g.CreatedAt = at.Time()
		out = append(out, g)
	}
	return out, rows.Err()
}
func ListEffectiveFederationPeerGrants(peer string) ([]FederationPeerGrant, error) {
	// A removed trust row cannot inherit authority even if a stale caller retained
	// its instance ID. Direct grants are already removed in the untrust transaction.
	p, err := GetFederationPeer(peer)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return []FederationPeerGrant{}, nil
	}
	direct, err := ListFederationPeerGrants(peer)
	if err != nil {
		return nil, err
	}
	inherited, err := listFederationPoolGrants("", peer)
	if err != nil {
		return nil, err
	}
	return append(direct, inherited...), nil
}
