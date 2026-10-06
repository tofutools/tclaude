package db

import (
	"encoding/json"
	"strconv"
	"time"
)

// FederationSpawnPolicy contains receiver-owned launch settings. Empty fields
// inherit ordinary operator group-spawn defaults.
type FederationSpawnPolicy struct {
	Profile string `json:"profile,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
	MaxLive int    `json:"max_live,omitempty"`
}

type FederationPeerGrant struct {
	Peer        string                `json:"peer"`
	Slug        string                `json:"slug"`
	Scope       string                `json:"scope"`
	SpawnPolicy FederationSpawnPolicy `json:"spawn_policy,omitempty"`
	CreatedAt   time.Time             `json:"created_at"`
}

func FederationGroupScope(groupID int64) string { return "group=" + strconv.FormatInt(groupID, 10) }

func UpsertFederationPeerGrant(g FederationPeerGrant) error {
	d, err := Open()
	if err != nil {
		return err
	}
	policy, err := json.Marshal(g.SpawnPolicy)
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO federation_peer_grants(peer,slug,scope,spawn_policy,created_at) VALUES(?,?,?,?,?)
 ON CONFLICT(peer,slug,scope) DO UPDATE SET spawn_policy=excluded.spawn_policy`, g.Peer, g.Slug, g.Scope, string(policy), dbTime(time.Now()))
	return err
}

func DeleteFederationPeerGrant(peer, slug, scope string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`DELETE FROM federation_peer_grants WHERE peer=? AND slug=? AND scope=?`, peer, slug, scope)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func ListFederationPeerGrants(peer string) ([]FederationPeerGrant, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT peer,slug,scope,spawn_policy,created_at FROM federation_peer_grants WHERE (?='' OR peer=?) ORDER BY peer,slug,scope`, peer, peer)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []FederationPeerGrant{}
	for rows.Next() {
		var g FederationPeerGrant
		var policy string
		var at dbTimestamp
		if err := rows.Scan(&g.Peer, &g.Slug, &g.Scope, &policy, &at); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(policy), &g.SpawnPolicy); err != nil {
			return nil, err
		}
		g.CreatedAt = at.Time()
		out = append(out, g)
	}
	return out, rows.Err()
}

// Auto workers survive daemon restarts; callers count only workers still live.
func RecordFederationAutoWorker(requestID int64, peer, agentID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO federation_auto_workers(request_id,peer,agent_id) VALUES(?,?,?)`, requestID, peer, agentID)
	return err
}
func ListFederationAutoWorkers(peer string) ([]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT agent_id FROM federation_auto_workers WHERE peer=?`, peer)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
