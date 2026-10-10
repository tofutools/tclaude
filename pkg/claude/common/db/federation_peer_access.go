package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
)

// FederationPeerAccessRequest associates an ordinary operator approval with a
// pinned peer. It never names a local agent as the permission beneficiary.
type FederationPeerAccessRequest struct {
	ID              string     `json:"id"`
	Peer            string     `json:"origin_peer"`
	Slug            string     `json:"perm"`
	GroupID         int64      `json:"group_id"`
	GrantGroupID    int64      `json:"grant_group_id"`
	GrantTTLSeconds int        `json:"grant_ttl_seconds"`
	ExpiresAt       *time.Time `json:"grant_expires_at,omitempty"`
	Status          string     `json:"status"`
}

func UpsertFederationPeerAccessRequest(r FederationPeerAccessRequest) error {
	d, err := Open()
	if err != nil {
		return err
	}
	var expires any
	if r.ExpiresAt != nil {
		expires = dbTime(*r.ExpiresAt)
	}
	_, err = d.Exec(`INSERT INTO federation_peer_access_requests(id,peer,slug,group_id,grant_group_id,grant_ttl_seconds,expires_at) VALUES(?,?,?,?,?,?,?)
 ON CONFLICT(id) DO UPDATE SET grant_group_id=excluded.grant_group_id,grant_ttl_seconds=excluded.grant_ttl_seconds,expires_at=excluded.expires_at`, r.ID, r.Peer, r.Slug, r.GroupID, r.GrantGroupID, r.GrantTTLSeconds, expires)
	return err
}
func GetFederationPeerAccessRequest(id string) (*FederationPeerAccessRequest, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var r FederationPeerAccessRequest
	var expires dbTimestamp
	err = d.QueryRow(`SELECT p.id,p.peer,p.slug,p.group_id,p.grant_group_id,p.grant_ttl_seconds,p.expires_at,COALESCE(a.status,'pending') FROM federation_peer_access_requests p LEFT JOIN access_requests a ON a.id=p.id WHERE p.id=?`, id).Scan(&r.ID, &r.Peer, &r.Slug, &r.GroupID, &r.GrantGroupID, &r.GrantTTLSeconds, &expires, &r.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !expires.Time().IsZero() {
		at := expires.Time()
		r.ExpiresAt = &at
	}
	return &r, nil
}

// ApproveFederationPeerAccessRequest rechecks all authority and records the
// grant in one transaction, serializing against untrust and scope revocation.
// Catalog lists come from the dispatcher and supported peer grants, not input.
func ApproveFederationPeerAccessRequest(r *FederationPeerAccessRequest, meaningfulSlugs, groupSlugs []string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var level string
	if err = tx.QueryRow(`SELECT trust_level FROM federation_peers WHERE instance_id=?`, r.Peer).Scan(&level); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	rows, err := tx.Query(`SELECT slug,scope,spawn_policy,expires_at,0 FROM federation_peer_grants WHERE peer=? AND (expires_at IS NULL OR expires_at>?)
 UNION ALL SELECT g.slug,g.scope,g.spawn_policy,NULL,1 FROM federation_node_group_grants g JOIN federation_node_group_members m ON m.group_id=g.group_id WHERE m.peer=?`, r.Peer, dbTime(time.Now()), r.Peer)
	if err != nil {
		return false, err
	}
	var grants []FederationPeerGrant
	for rows.Next() {
		var g FederationPeerGrant
		var policy string
		var expires dbTimestamp
		var inherited int
		if err = rows.Scan(&g.Slug, &g.Scope, &policy, &expires, &inherited); err != nil {
			_ = rows.Close()
			return false, err
		}
		if err = json.Unmarshal([]byte(policy), &g.SpawnPolicy); err != nil {
			_ = rows.Close()
			return false, err
		}
		if inherited != 0 {
			g.PoolID = "inherited"
		}
		if !expires.Time().IsZero() {
			at := expires.Time()
			g.ExpiresAt = &at
		}
		grants = append(grants, g)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return false, err
	}
	activeGroup := func(id int64) bool {
		var archived dbTimestamp
		return tx.QueryRow(`SELECT archived_at FROM agent_groups WHERE id=?`, id).Scan(&archived) == nil && archived.Time().IsZero()
	}
	admitted := level == FederationTrustUnrestricted
	visible := admitted
	for _, g := range grants {
		if !slices.Contains(meaningfulSlugs, g.Slug) {
			continue
		}
		valid := g.Scope == ""
		if strings.HasPrefix(g.Scope, "group=") {
			id, e := strconv.ParseInt(strings.TrimPrefix(g.Scope, "group="), 10, 64)
			valid = e == nil && activeGroup(id)
		}
		// Model proxy scopes confer peer access, but never group visibility.
		if strings.HasPrefix(g.Scope, "http_proxy=") {
			valid = true
		}
		admitted = admitted || valid
		if valid && slices.Contains(groupSlugs, g.Slug) && (g.Scope == "" || g.Scope == FederationGroupScope(r.GrantGroupID)) {
			visible = true
		}
	}
	if !admitted || r.GrantGroupID != 0 && (!activeGroup(r.GrantGroupID) || !visible) {
		return false, nil
	}
	scope := ""
	if r.GrantGroupID != 0 {
		scope = FederationGroupScope(r.GrantGroupID)
	}
	var wanted *time.Time
	if r.GrantTTLSeconds > 0 {
		at := time.Now().Add(time.Duration(r.GrantTTLSeconds) * time.Second)
		wanted = &at
	}
	covered := level == FederationTrustUnrestricted
	var policy FederationSpawnPolicy
	bestRank := -1
	for _, g := range grants {
		if g.Slug != r.Slug || g.Scope != "" && g.Scope != scope {
			continue
		}
		rank := 0
		if g.Scope != "" {
			rank += 2
		}
		if g.PoolID == "" {
			rank++
		}
		if rank > bestRank {
			policy = g.SpawnPolicy
			bestRank = rank
		}
		if g.ExpiresAt == nil || wanted != nil && !g.ExpiresAt.Before(*wanted) {
			covered = true
			r.ExpiresAt = g.ExpiresAt
		}
	}
	if !covered {
		raw, e := json.Marshal(policy)
		if e != nil {
			return false, e
		}
		var expires any
		if wanted != nil {
			expires = dbTime(*wanted)
		}
		_, err = tx.Exec(`INSERT INTO federation_peer_grants(peer,slug,scope,spawn_policy,created_at,expires_at) VALUES(?,?,?,?,?,?)
 ON CONFLICT(peer,slug,scope) DO UPDATE SET expires_at=CASE WHEN federation_peer_grants.expires_at IS NULL OR excluded.expires_at IS NULL THEN NULL ELSE MAX(federation_peer_grants.expires_at,excluded.expires_at) END`, r.Peer, r.Slug, scope, string(raw), dbTime(time.Now()), expires)
		if err != nil {
			return false, err
		}
		r.ExpiresAt = wanted
	}
	var expires any
	if r.ExpiresAt != nil {
		expires = dbTime(*r.ExpiresAt)
	}
	_, err = tx.Exec(`UPDATE federation_peer_access_requests SET grant_group_id=?,grant_ttl_seconds=?,expires_at=? WHERE id=? AND peer=?`, r.GrantGroupID, r.GrantTTLSeconds, expires, r.ID, r.Peer)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
