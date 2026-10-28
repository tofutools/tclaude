package db

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

const hostRouteIDPrefix = "rte_host_"

func IsHostRouteID(id string) bool { return strings.HasPrefix(id, hostRouteIDPrefix) }

// Host-only publications deliberately have no group FK or agent consumer lease.
// Reusing the route projection keeps the authenticated publisher data plane common.
const hostRouteColumns = `id, 0, '', publisher_agent_id, publisher_conv_id,
 publisher_launch_generation, 1, name, transport, target, state,
 created_at, withdrawn_at, withdraw_reason`

func CreateHostAgentRoute(agentID, convID, generation, name, transport, target string) (*AgentRoute, error) {
	if agentID == "" || convID == "" || generation == "" || name == "" || transport == "" || target == "" {
		return nil, errors.New("host route identity, name, transport and target required")
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var current string
	if err := tx.QueryRow(`SELECT current_conv_id FROM agents WHERE agent_id = ? AND retired_at IS NULL`, agentID).Scan(&current); err != nil {
		return nil, err
	}
	if current != convID {
		return nil, errors.New("publisher conversation is no longer current")
	}
	// A completed publication must not permanently reserve its name across
	// publisher relaunches. Keep active-name conflicts explicit.
	if _, err := tx.Exec(`DELETE FROM agent_host_routes WHERE publisher_agent_id=? AND name=? AND state != ?`, agentID, name, RouteStateReady); err != nil {
		return nil, err
	}
	id := hostRouteIDPrefix + strings.TrimPrefix(newRouteID(), "rte_")
	now := time.Now()
	if _, err := tx.Exec(`INSERT INTO agent_host_routes (id,publisher_agent_id,publisher_conv_id,publisher_launch_generation,name,transport,target,state,created_at) VALUES (?,?,?,?,?,?,?,?,?)`, id, agentID, convID, generation, name, transport, target, RouteStateReady, dbTime(now)); err != nil {
		return nil, err
	}
	if err := insertRouteAuditTx(tx, now, "publish", "ok", 0, id, "", agentID, convID, "host-only"); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return GetAgentRoute(id)
}

func getHostAgentRoute(id string) (*AgentRoute, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	route, err := scanAgentRoute(d.QueryRow(`SELECT `+hostRouteColumns+` FROM agent_host_routes WHERE id = ?`, id))
	if route != nil {
		route.HostOnly = true
	}
	return route, err
}

// Empty publisher selects the operator view; agent callers must supply their identity.
func ListHostAgentRoutes(publisher string) ([]*AgentRoute, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT `+hostRouteColumns+` FROM agent_host_routes WHERE (? = '' OR publisher_agent_id = ?) ORDER BY created_at,id`, publisher, publisher)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	routes := []*AgentRoute{}
	for rows.Next() {
		r, err := scanAgentRoute(rows)
		if err != nil {
			return nil, err
		}
		r.HostOnly = true
		routes = append(routes, r)
	}
	return routes, rows.Err()
}

func transitionHostAgentRoute(id, owner, conv, state, reason string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var agentID, convID, current string
	if err := tx.QueryRow(`SELECT publisher_agent_id,publisher_conv_id,state FROM agent_host_routes WHERE id=?`, id).Scan(&agentID, &convID, &current); err != nil {
		return err
	}
	if owner != "" && (owner != agentID || conv != convID) {
		return errors.New("only current publisher may withdraw host route")
	}
	if current != RouteStateReady {
		return nil
	}
	now := time.Now()
	if _, err := tx.Exec(`UPDATE agent_host_routes SET state=?,withdrawn_at=?,withdraw_reason=? WHERE id=?`, state, dbTime(now), reason, id); err != nil {
		return err
	}
	if err := insertRouteAuditTx(tx, now, state, "ok", 0, id, "", agentID, convID, reason); err != nil {
		return err
	}
	return tx.Commit()
}

func markHostAgentRoutesPublisherLostTx(tx *sql.Tx, conv, generation, reason string) error {
	_, err := tx.Exec(`INSERT INTO agent_route_audit(at,action,result,group_id,route_id,actor_agent_id,actor_conv_id,detail)
 SELECT ?, 'publisher-lost','ok',NULL,id,publisher_agent_id,publisher_conv_id,? FROM agent_host_routes WHERE publisher_conv_id=? AND state='ready' AND (?='' OR publisher_launch_generation=?)`, dbTime(time.Now()), reason, conv, generation, generation)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE agent_host_routes SET state='publisher-lost',withdrawn_at=?,withdraw_reason=? WHERE publisher_conv_id=? AND state='ready' AND (?='' OR publisher_launch_generation=?)`, dbTime(time.Now()), reason, conv, generation, generation)
	return err
}
