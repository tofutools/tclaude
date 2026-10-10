package db

import (
	"database/sql"
	"errors"
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

// EnsureFederationPeerGrant is for approvals. It only adds authority: an
// existing permanent or longer-lived grant and receiver launch policy survive.
func EnsureFederationPeerGrant(g FederationPeerGrant) error {
	d, err := Open()
	if err != nil {
		return err
	}
	var expires any
	if g.ExpiresAt != nil {
		expires = dbTime(*g.ExpiresAt)
	}
	_, err = d.Exec(`INSERT INTO federation_peer_grants(peer,slug,scope,spawn_policy,created_at,expires_at) VALUES(?,?,?,'{}',?,?)
 ON CONFLICT(peer,slug,scope) DO UPDATE SET expires_at=CASE
 WHEN federation_peer_grants.expires_at IS NULL OR excluded.expires_at IS NULL THEN NULL
 ELSE MAX(federation_peer_grants.expires_at,excluded.expires_at) END`, g.Peer, g.Slug, g.Scope, dbTime(time.Now()), expires)
	return err
}
