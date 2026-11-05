package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Peer agent view policies: whether this node serves peer views a trusted
// peer's agents open (agent principals). The default, with no row, is allow;
// the peer's own grants still bound every request.
const (
	FederationPeerAgentViewsAllow = "allow"
	FederationPeerAgentViewsDeny  = "deny"
)

// GetFederationPeerAgentViews returns peer's agent view policy, allow when
// none is set.
func GetFederationPeerAgentViews(peer string) (string, error) {
	d, err := Open()
	if err != nil {
		return "", err
	}
	var policy string
	err = d.QueryRow(`SELECT policy FROM federation_peer_agent_views WHERE peer=?`, peer).Scan(&policy)
	if errors.Is(err, sql.ErrNoRows) {
		return FederationPeerAgentViewsAllow, nil
	}
	if err != nil {
		return "", err
	}
	return policy, nil
}

// SetFederationPeerAgentViews stores peer's agent view policy.
func SetFederationPeerAgentViews(peer, policy string) error {
	if policy != FederationPeerAgentViewsAllow && policy != FederationPeerAgentViewsDeny {
		return fmt.Errorf("invalid agent view policy %q", policy)
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO federation_peer_agent_views(peer, policy, updated_at) VALUES(?,?,?)
		ON CONFLICT(peer) DO UPDATE SET policy=excluded.policy, updated_at=excluded.updated_at`, peer, policy, dbTime(time.Now()))
	return err
}
