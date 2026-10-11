package db

import (
	"database/sql"
	"errors"
	"time"
)

// FederationRouteMirror marks a local route row standing in for a remote
// route (see migrateV231toV232).
type FederationRouteMirror struct {
	RouteID     string
	Peer        string
	RemoteRoute string
	RemoteLabel string
	// GroupID is the local group holding the mirror route. Only
	// ListFederationRouteMirrors fills it.
	GroupID int64
}

// CreateFederationRouteMirror creates the mirror route row and its marker in
// one step: the row is never visible unmarked.
func CreateFederationRouteMirror(groupID int64, agentID, convID, launchGeneration string, groupGeneration int64, name string, m FederationRouteMirror) (*AgentRoute, error) {
	// Route names stay unique per publisher across states, so a mirror
	// withdrawn earlier (restart, revocation, relaunch) would block its
	// own reopen. Mirrors keep no history worth that: drop them.
	if err := deleteStaleFederationRouteMirrors(`AND r.group_id = ? AND r.publisher_agent_id = ? AND r.name = ?`, groupID, agentID, name); err != nil {
		return nil, err
	}
	return createAgentRoute(groupID, agentID, convID, launchGeneration, groupGeneration, name, "tcp", "federation://"+m.Peer+"/"+m.RemoteRoute,
		func(tx *sql.Tx, route *AgentRoute) error {
			_, err := tx.Exec(`INSERT INTO federation_route_mirrors(route_id, peer, remote_route, remote_label, created_at) VALUES(?,?,?,?,?)`,
				route.ID, m.Peer, m.RemoteRoute, m.RemoteLabel, dbTime(time.Now()))
			return err
		})
}

// GetFederationRouteMirror returns the marker for routeID, or nil.
func GetFederationRouteMirror(routeID string) (*FederationRouteMirror, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var m FederationRouteMirror
	err = d.QueryRow(`SELECT route_id, peer, remote_route, remote_label FROM federation_route_mirrors WHERE route_id=?`, routeID).
		Scan(&m.RouteID, &m.Peer, &m.RemoteRoute, &m.RemoteLabel)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &m, err
}

// FindFederationRouteMirror returns the ready mirror of (peer, remote route)
// owned by agentID in groupID, or nil.
func FindFederationRouteMirror(groupID int64, agentID, peer, remoteRoute string) (*AgentRoute, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var id string
	err = d.QueryRow(`SELECT r.id FROM agent_routes r JOIN federation_route_mirrors m ON m.route_id = r.id
		WHERE r.group_id=? AND r.publisher_agent_id=? AND m.peer=? AND m.remote_route=? AND r.state=?
		ORDER BY r.created_at DESC LIMIT 1`, groupID, agentID, peer, remoteRoute, RouteStateReady).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return GetAgentRoute(id)
}

// ListFederationRouteMirrors returns every marker whose route is ready.
func ListFederationRouteMirrors() ([]FederationRouteMirror, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT m.route_id, m.peer, m.remote_route, m.remote_label, r.group_id FROM federation_route_mirrors m
		JOIN agent_routes r ON r.id = m.route_id WHERE r.state = ?`, RouteStateReady)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []FederationRouteMirror
	for rows.Next() {
		var m FederationRouteMirror
		if err := rows.Scan(&m.RouteID, &m.Peer, &m.RemoteRoute, &m.RemoteLabel, &m.GroupID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// OpenFederationProxyLease opens the publisher-side proxy lease for peer on
// routeID, held in the route publisher's name, and marks it.
func OpenFederationProxyLease(route *AgentRoute, peer string) (*AgentRouteLease, error) {
	return openAgentRouteLease(route.ID, route.PublisherAgentID, route.PublisherConvID, route.PublisherLaunchGeneration, route.GroupGeneration,
		func(tx *sql.Tx, lease *AgentRouteLease) error {
			_, err := tx.Exec(`INSERT INTO federation_route_proxies(lease_id, peer, route_id, created_at) VALUES(?,?,?,?)`,
				lease.ID, peer, route.ID, dbTime(time.Now()))
			return err
		})
}

// IsFederationProxyLease reports whether leaseID is a publisher-side proxy.
func IsFederationProxyLease(leaseID string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var n int
	err = d.QueryRow(`SELECT COUNT(*) FROM federation_route_proxies WHERE lease_id=?`, leaseID).Scan(&n)
	return n > 0, err
}

// DeleteStaleFederationRouteMirrors removes every mirror that is no longer
// ready, with its leases and marker.
func DeleteStaleFederationRouteMirrors() error {
	return deleteStaleFederationRouteMirrors("")
}

func deleteStaleFederationRouteMirrors(filter string, args ...any) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM agent_routes WHERE id IN (
		SELECT r.id FROM agent_routes r JOIN federation_route_mirrors m ON m.route_id = r.id
		WHERE r.state != ? `+filter+`)`, append([]any{RouteStateReady}, args...)...)
	return err
}
