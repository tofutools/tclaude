package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	RouteStateReady         = "ready"
	RouteStateDraining      = "draining"
	RouteStateWithdrawn     = "withdrawn"
	RouteStatePublisherLost = "publisher-lost"
	RouteLeaseOpen          = "open"
	RouteLeaseClosed        = "closed"
)

// AgentRoute is the durable authority record for a named group route. The
// data plane deliberately does not appear here; adapters consume this
// generation-bound record in a later milestone.
type AgentRoute struct {
	ID                        string
	GroupID                   int64
	GroupName                 string
	PublisherAgentID          string
	PublisherConvID           string
	PublisherLaunchGeneration string
	GroupGeneration           int64
	Name                      string
	Transport                 string
	Target                    string
	State                     string
	CreatedAt                 time.Time
	WithdrawnAt               time.Time
	WithdrawReason            string
}

type AgentRouteLease struct {
	ID                       string
	RouteID                  string
	ConsumerAgentID          string
	ConsumerConvID           string
	ConsumerLaunchGeneration string
	GroupGeneration          int64
	State                    string
	OpenedAt                 time.Time
	ClosedAt                 time.Time
}

func newRouteID() string      { return "rte_" + strings.ReplaceAll(uuid.NewString(), "-", "") }
func newRouteLeaseID() string { return "rlease_" + strings.ReplaceAll(uuid.NewString(), "-", "") }

// CreateAgentRoute creates a ready route only when the publisher is a current
// member of the selected active group and the supplied epoch is current.
func CreateAgentRoute(groupID int64, publisherAgentID, publisherConvID, launchGeneration string, groupGeneration int64, name, transport, target string) (*AgentRoute, error) {
	if strings.TrimSpace(publisherAgentID) == "" || strings.TrimSpace(launchGeneration) == "" {
		return nil, errors.New("publisher agent and launch generation are required")
	}
	if strings.TrimSpace(name) == "" || strings.TrimSpace(transport) == "" || strings.TrimSpace(target) == "" {
		return nil, errors.New("route name, transport, and target are required")
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
	var archived sql.NullInt64
	var currentGeneration int64
	var groupName string
	if err := tx.QueryRow(`SELECT name, archived_at, route_generation FROM agent_groups WHERE id = ?`, groupID).Scan(&groupName, &archived, &currentGeneration); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	if archived.Valid {
		return nil, fmt.Errorf("group is archived")
	}
	if currentGeneration != groupGeneration {
		return nil, fmt.Errorf("stale group generation: expected %d, current %d", groupGeneration, currentGeneration)
	}
	var memberCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agent_group_members m JOIN agents a ON a.agent_id = m.agent_id AND a.retired_at IS NULL WHERE m.group_id = ? AND m.agent_id = ?`, groupID, publisherAgentID).Scan(&memberCount); err != nil {
		return nil, err
	}
	if memberCount == 0 {
		return nil, fmt.Errorf("publisher is not a member of group")
	}
	created := time.Now()
	route := &AgentRoute{ID: newRouteID(), GroupID: groupID, GroupName: groupName, PublisherAgentID: publisherAgentID, PublisherConvID: publisherConvID, PublisherLaunchGeneration: launchGeneration, GroupGeneration: groupGeneration, Name: name, Transport: transport, Target: target, State: RouteStateReady, CreatedAt: created}
	if _, err := tx.Exec(`INSERT INTO agent_routes
		(id, group_id, publisher_agent_id, publisher_conv_id, publisher_launch_generation, group_generation, name, transport, target, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		route.ID, route.GroupID, route.PublisherAgentID, route.PublisherConvID, route.PublisherLaunchGeneration,
		route.GroupGeneration, route.Name, route.Transport, route.Target, route.State, dbTime(created)); err != nil {
		return nil, err
	}
	if err := insertRouteAuditTx(tx, created, "publish", "ok", groupID, route.ID, "", publisherAgentID, publisherConvID, ""); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return route, nil
}

// GetAgentRoute returns one route, including the stable group name snapshot
// resolved from the current group row (display names never form identity).
func GetAgentRoute(routeID string) (*AgentRoute, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`SELECT r.id, r.group_id, g.name, r.publisher_agent_id, r.publisher_conv_id,
		r.publisher_launch_generation, r.group_generation, r.name, r.transport, r.target, r.state,
		r.created_at, r.withdrawn_at, r.withdraw_reason
		FROM agent_routes r JOIN agent_groups g ON g.id = r.group_id WHERE r.id = ?`, routeID)
	return scanAgentRoute(row)
}

func ListAgentRoutes(groupID int64) ([]*AgentRoute, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT r.id, r.group_id, g.name, r.publisher_agent_id, r.publisher_conv_id,
		r.publisher_launch_generation, r.group_generation, r.name, r.transport, r.target, r.state,
		r.created_at, r.withdrawn_at, r.withdraw_reason
		FROM agent_routes r JOIN agent_groups g ON g.id = r.group_id
		WHERE r.group_id = ? ORDER BY r.created_at, r.id`, groupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*AgentRoute
	for rows.Next() {
		route, err := scanAgentRoute(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, route)
	}
	return out, rows.Err()
}

// DashboardRouteHistoryPerGroup is the hard projection bound for route
// registry history. Current routes are retained first; only the newest
// terminal rows consume the remainder of this per-group budget.
const DashboardRouteHistoryPerGroup = 64

// DashboardRouteLeaseHistoryPerRoute is the hard projection bound for lease
// history. Every open lease is retained, followed by the newest closed leases
// for each retained route.
const DashboardRouteLeaseHistoryPerRoute = 8

// ListAgentRoutesBatch returns a bounded route registry projection grouped by
// active group. Current routes are retained before terminal history, so a
// route that is currently ready or draining cannot be evicted by old cycles.
func ListAgentRoutesBatch(groupIDs []int64) (map[int64][]*AgentRoute, error) {
	out := make(map[int64][]*AgentRoute, len(groupIDs))
	if len(groupIDs) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	placeholders := make([]string, len(groupIDs))
	args := make([]any, len(groupIDs))
	for i, groupID := range groupIDs {
		placeholders[i] = "?"
		args[i] = groupID
	}
	queryArgs := []any{RouteStateReady, RouteStateDraining}
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, DashboardRouteHistoryPerGroup, RouteStateReady, RouteStateDraining)
	rows, err := d.Query(`WITH ranked_routes AS (
		SELECT r.id, r.group_id, g.name AS group_name, r.publisher_agent_id,
			r.publisher_conv_id, r.publisher_launch_generation, r.group_generation,
			r.name AS route_name, r.transport, r.target, r.state, r.created_at, r.withdrawn_at,
			r.withdraw_reason,
			ROW_NUMBER() OVER (
				PARTITION BY r.group_id
				ORDER BY CASE WHEN r.state IN (?, ?) THEN 0 ELSE 1 END,
					r.created_at DESC, r.id DESC
			) AS projection_rank
		FROM agent_routes r JOIN agent_groups g ON g.id = r.group_id
		WHERE r.group_id IN (`+strings.Join(placeholders, ",")+
		`)
	)
	SELECT id, group_id, group_name, publisher_agent_id, publisher_conv_id,
		publisher_launch_generation, group_generation, route_name, transport, target,
		state, created_at, withdrawn_at, withdraw_reason
	FROM ranked_routes
	WHERE projection_rank <= ?
	ORDER BY group_id, CASE WHEN state IN (?, ?) THEN 0 ELSE 1 END,
		created_at DESC, id DESC`, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		route, scanErr := scanAgentRoute(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out[route.GroupID] = append(out[route.GroupID], route)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// WithdrawAgentRoute is owner-scoped and idempotent for an already withdrawn
// route. It also closes all outstanding leases so a withdrawn route cannot
// leave stale consumer authority behind.
func WithdrawAgentRoute(routeID, publisherAgentID, publisherConvID, reason string) error {
	return transitionAgentRoute(routeID, publisherAgentID, publisherConvID, RouteStateWithdrawn, reason, "withdraw")
}

func MarkAgentRoutePublisherLost(routeID, reason string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var groupID int64
	var publisherAgent, publisherConv, state string
	if err := tx.QueryRow(`SELECT group_id, publisher_agent_id, publisher_conv_id, state FROM agent_routes WHERE id = ?`, routeID).Scan(&groupID, &publisherAgent, &publisherConv, &state); err != nil {
		return err
	}
	if state == RouteStateWithdrawn || state == RouteStatePublisherLost {
		return nil
	}
	now := time.Now()
	if _, err := tx.Exec(`UPDATE agent_routes SET state = ?, withdrawn_at = ?, withdraw_reason = ? WHERE id = ? AND state IN (?, ?)`, RouteStatePublisherLost, dbTime(now), reason, routeID, RouteStateReady, RouteStateDraining); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE agent_route_leases SET state = ?, closed_at = ? WHERE route_id = ? AND state = ?`, RouteLeaseClosed, dbTime(now), routeID, RouteLeaseOpen); err != nil {
		return err
	}
	if err := insertRouteAuditTx(tx, now, "publisher-lost", "ok", groupID, routeID, "", publisherAgent, publisherConv, reason); err != nil {
		return err
	}
	return tx.Commit()
}

// markAgentRoutesPublisherLostTx withdraws every ready route owned by one
// publisher launch and closes its open leases. It runs inside the
// authoritative session-exit transaction, so a normal process exit cannot
// leave a consumable route behind while the reaper or SessionEnd hook settles
// the session row.
func markAgentRoutesPublisherLostTx(tx *sql.Tx, publisherConvID, launchGeneration, reason string) error {
	if strings.TrimSpace(publisherConvID) == "" {
		return nil
	}
	rows, err := tx.Query(`SELECT id, group_id, publisher_agent_id, publisher_conv_id
		FROM agent_routes
		WHERE publisher_conv_id = ? AND state IN (?, ?)
			AND (? = '' OR publisher_launch_generation = ?)`,
		publisherConvID, RouteStateReady, RouteStateDraining, launchGeneration, launchGeneration)
	if err != nil {
		return err
	}
	type routeOwner struct {
		id, publisherAgent, publisherConv string
		groupID                           int64
	}
	var routes []routeOwner
	for rows.Next() {
		var route routeOwner
		if err := rows.Scan(&route.id, &route.groupID, &route.publisherAgent, &route.publisherConv); err != nil {
			_ = rows.Close()
			return err
		}
		routes = append(routes, route)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	now := time.Now()
	for _, route := range routes {
		if _, err := tx.Exec(`UPDATE agent_routes
			SET state = ?, withdrawn_at = ?, withdraw_reason = ?
			WHERE id = ? AND state IN (?, ?)`,
			RouteStatePublisherLost, dbTime(now), reason, route.id, RouteStateReady, RouteStateDraining); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE agent_route_leases
			SET state = ?, closed_at = ?
			WHERE route_id = ? AND state = ?`, RouteLeaseClosed, dbTime(now), route.id, RouteLeaseOpen); err != nil {
			return err
		}
		if err := insertRouteAuditTx(tx, now, "publisher-lost", "ok", route.groupID, route.id, "", route.publisherAgent, route.publisherConv, reason); err != nil {
			return err
		}
	}
	return nil
}

// markAgentRouteConsumerLeasesLostTx closes every open lease owned by the
// exiting consumer conversation and launch generation. Keeping the stable
// actor, conversation, and launch generation in the predicate prevents an
// exit callback from an older generation from revoking a relaunch's lease.
// It runs in the same authoritative transaction as the session-exit CAS.
func markAgentRouteConsumerLeasesLostTx(tx *sql.Tx, consumerAgentID, consumerConvID, launchGeneration, reason string) error {
	if strings.TrimSpace(consumerAgentID) == "" || strings.TrimSpace(consumerConvID) == "" {
		return nil
	}
	rows, err := tx.Query(`SELECT l.id, l.route_id, r.group_id, l.consumer_agent_id, l.consumer_conv_id
		FROM agent_route_leases l JOIN agent_routes r ON r.id = l.route_id
		WHERE l.consumer_agent_id = ? AND l.consumer_conv_id = ? AND l.state = ?
			AND (? = '' OR l.consumer_launch_generation = ?)`,
		consumerAgentID, consumerConvID, RouteLeaseOpen, launchGeneration, launchGeneration)
	if err != nil {
		return err
	}
	type leaseOwner struct {
		id, routeID, consumerAgent, consumerConv string
		groupID                                  int64
	}
	var leases []leaseOwner
	for rows.Next() {
		var lease leaseOwner
		if err := rows.Scan(&lease.id, &lease.routeID, &lease.groupID, &lease.consumerAgent, &lease.consumerConv); err != nil {
			_ = rows.Close()
			return err
		}
		leases = append(leases, lease)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	now := time.Now()
	for _, lease := range leases {
		if _, err := tx.Exec(`UPDATE agent_route_leases
			SET state = ?, closed_at = ?
			WHERE id = ? AND state = ?`, RouteLeaseClosed, dbTime(now), lease.id, RouteLeaseOpen); err != nil {
			return err
		}
		if err := insertRouteAuditTx(tx, now, "consumer-lost", "ok", lease.groupID, lease.routeID, lease.id, lease.consumerAgent, lease.consumerConv, reason); err != nil {
			return err
		}
	}
	return nil
}

func OpenAgentRouteLease(routeID, consumerAgentID, consumerConvID, launchGeneration string, groupGeneration int64) (*AgentRouteLease, error) {
	if strings.TrimSpace(consumerAgentID) == "" || strings.TrimSpace(launchGeneration) == "" {
		return nil, errors.New("consumer agent and launch generation are required")
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
	var groupID int64
	var routeState string
	var routeGeneration int64
	if err := tx.QueryRow(`SELECT group_id, state, group_generation FROM agent_routes WHERE id = ?`, routeID).Scan(&groupID, &routeState, &routeGeneration); err != nil {
		return nil, err
	}
	if routeState != RouteStateReady {
		return nil, fmt.Errorf("route is not ready: %s", routeState)
	}
	var currentGeneration int64
	if err := tx.QueryRow(`SELECT route_generation FROM agent_groups WHERE id = ? AND archived_at IS NULL`, groupID).Scan(&currentGeneration); err != nil {
		return nil, err
	}
	if currentGeneration != groupGeneration {
		return nil, fmt.Errorf("stale group generation: expected %d, current %d", groupGeneration, currentGeneration)
	}
	if routeGeneration != currentGeneration {
		return nil, fmt.Errorf("route has stale group generation: route %d, current %d", routeGeneration, currentGeneration)
	}
	var members int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agent_group_members m JOIN agents a ON a.agent_id = m.agent_id AND a.retired_at IS NULL WHERE m.group_id = ? AND m.agent_id = ?`, groupID, consumerAgentID).Scan(&members); err != nil {
		return nil, err
	}
	if members == 0 {
		return nil, fmt.Errorf("consumer is not a member of group")
	}
	now := time.Now()
	lease := &AgentRouteLease{ID: newRouteLeaseID(), RouteID: routeID, ConsumerAgentID: consumerAgentID, ConsumerConvID: consumerConvID, ConsumerLaunchGeneration: launchGeneration, GroupGeneration: groupGeneration, State: RouteLeaseOpen, OpenedAt: now}
	if _, err := tx.Exec(`INSERT INTO agent_route_leases
		(id, route_id, consumer_agent_id, consumer_conv_id, consumer_launch_generation, group_generation, state, opened_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, lease.ID, lease.RouteID, lease.ConsumerAgentID, lease.ConsumerConvID, lease.ConsumerLaunchGeneration, lease.GroupGeneration, lease.State, dbTime(now)); err != nil {
		return nil, err
	}
	if err := insertRouteAuditTx(tx, now, "consume", "ok", groupID, routeID, lease.ID, consumerAgentID, consumerConvID, ""); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return lease, nil
}

func GetAgentRouteLease(leaseID string) (*AgentRouteLease, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`SELECT id, route_id, consumer_agent_id, consumer_conv_id,
		consumer_launch_generation, group_generation, state, opened_at, closed_at
		FROM agent_route_leases WHERE id = ?`, leaseID)
	var lease AgentRouteLease
	var openedAt, closedAt dbTimestamp
	if err := row.Scan(&lease.ID, &lease.RouteID, &lease.ConsumerAgentID, &lease.ConsumerConvID, &lease.ConsumerLaunchGeneration, &lease.GroupGeneration, &lease.State, &openedAt, &closedAt); err != nil {
		return nil, err
	}
	lease.OpenedAt = openedAt.Time()
	lease.ClosedAt = closedAt.Time()
	return &lease, nil
}

// ListAgentRouteLeases returns open and historical leases for one consumer in
// a selected group. The group predicate is part of the query so a helper
// polling multiple groups cannot accidentally observe a lease from another
// route-enabled group.
func ListAgentRouteLeases(groupID int64, consumerAgentID, consumerConvID string) ([]*AgentRouteLease, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT l.id, l.route_id, l.consumer_agent_id, l.consumer_conv_id,
		l.consumer_launch_generation, l.group_generation, l.state, l.opened_at, l.closed_at
		FROM agent_route_leases l JOIN agent_routes r ON r.id = l.route_id
		WHERE r.group_id = ? AND l.consumer_agent_id = ? AND l.consumer_conv_id = ?
		ORDER BY l.opened_at, l.id`, groupID, consumerAgentID, consumerConvID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*AgentRouteLease
	for rows.Next() {
		var lease AgentRouteLease
		var openedAt, closedAt dbTimestamp
		if err := rows.Scan(&lease.ID, &lease.RouteID, &lease.ConsumerAgentID, &lease.ConsumerConvID,
			&lease.ConsumerLaunchGeneration, &lease.GroupGeneration, &lease.State, &openedAt, &closedAt); err != nil {
			return nil, err
		}
		lease.OpenedAt = openedAt.Time()
		lease.ClosedAt = closedAt.Time()
		out = append(out, &lease)
	}
	return out, rows.Err()
}

// ListAgentRouteLeasesBatch returns a bounded lease projection grouped by route
// group. All open leases survive, followed by the newest closed leases for
// each retained route. The retained-route CTE prevents old route history from
// widening the dashboard payload indefinitely.
func ListAgentRouteLeasesBatch(groupIDs []int64) (map[int64][]*AgentRouteLease, error) {
	out := make(map[int64][]*AgentRouteLease, len(groupIDs))
	if len(groupIDs) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	placeholders := make([]string, len(groupIDs))
	args := make([]any, len(groupIDs))
	for i, groupID := range groupIDs {
		placeholders[i] = "?"
		args[i] = groupID
	}
	queryArgs := []any{RouteStateReady, RouteStateDraining}
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, DashboardRouteHistoryPerGroup, RouteLeaseOpen, DashboardRouteLeaseHistoryPerRoute)
	rows, err := d.Query(`WITH ranked_routes AS (
		SELECT r.id, r.group_id,
			ROW_NUMBER() OVER (
				PARTITION BY r.group_id
				ORDER BY CASE WHEN r.state IN (?, ?) THEN 0 ELSE 1 END,
					r.created_at DESC, r.id DESC
			) AS projection_rank
		FROM agent_routes r
		WHERE r.group_id IN (`+strings.Join(placeholders, ",")+
		`)), retained_routes AS (
		SELECT id, group_id FROM ranked_routes
		WHERE projection_rank <= ?
		), ranked_leases AS (
		SELECT rr.group_id, l.id, l.route_id, l.consumer_agent_id,
			l.consumer_conv_id, l.consumer_launch_generation, l.group_generation,
			l.state, l.opened_at, l.closed_at,
			ROW_NUMBER() OVER (
				PARTITION BY l.route_id
				ORDER BY CASE WHEN l.state = ? THEN 0 ELSE 1 END,
					l.opened_at DESC, l.id DESC
			) AS projection_rank
		FROM agent_route_leases l JOIN retained_routes rr ON rr.id = l.route_id
		)
		SELECT group_id, id, route_id, consumer_agent_id, consumer_conv_id,
			consumer_launch_generation, group_generation, state, opened_at, closed_at
		FROM ranked_leases
		WHERE projection_rank <= ?
		ORDER BY group_id, route_id, CASE WHEN state = ? THEN 0 ELSE 1 END,
			opened_at DESC, id DESC`, append(queryArgs, RouteLeaseOpen)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var groupID int64
		var lease AgentRouteLease
		var openedAt, closedAt dbTimestamp
		if err := rows.Scan(&groupID, &lease.ID, &lease.RouteID, &lease.ConsumerAgentID,
			&lease.ConsumerConvID, &lease.ConsumerLaunchGeneration, &lease.GroupGeneration,
			&lease.State, &openedAt, &closedAt); err != nil {
			return nil, err
		}
		lease.OpenedAt = openedAt.Time()
		lease.ClosedAt = closedAt.Time()
		out[groupID] = append(out[groupID], &lease)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func CloseAgentRouteLease(leaseID, consumerAgentID, consumerConvID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var routeID string
	var groupID int64
	var owner, state string
	if err := tx.QueryRow(`SELECT l.route_id, r.group_id, l.consumer_agent_id, l.state FROM agent_route_leases l JOIN agent_routes r ON r.id = l.route_id WHERE l.id = ?`, leaseID).Scan(&routeID, &groupID, &owner, &state); err != nil {
		return err
	}
	if owner != consumerAgentID {
		return fmt.Errorf("lease is owned by another agent")
	}
	if state == RouteLeaseClosed {
		return nil
	}
	now := time.Now()
	if _, err := tx.Exec(`UPDATE agent_route_leases SET state = ?, closed_at = ? WHERE id = ? AND consumer_agent_id = ? AND state = ?`, RouteLeaseClosed, dbTime(now), leaseID, consumerAgentID, RouteLeaseOpen); err != nil {
		return err
	}
	if err := insertRouteAuditTx(tx, now, "close-lease", "ok", groupID, routeID, leaseID, consumerAgentID, consumerConvID, ""); err != nil {
		return err
	}
	return tx.Commit()
}

func transitionAgentRoute(routeID, publisherAgentID, publisherConvID, state, reason, action string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var groupID int64
	var owner, current string
	if err := tx.QueryRow(`SELECT group_id, publisher_agent_id, state FROM agent_routes WHERE id = ?`, routeID).Scan(&groupID, &owner, &current); err != nil {
		return err
	}
	if owner != publisherAgentID {
		return fmt.Errorf("route is owned by another agent")
	}
	if current == state || current == RouteStateWithdrawn || current == RouteStatePublisherLost {
		return nil
	}
	now := time.Now()
	if _, err := tx.Exec(`UPDATE agent_routes SET state = ?, withdrawn_at = ?, withdraw_reason = ? WHERE id = ? AND publisher_agent_id = ? AND state IN (?, ?)`, state, dbTime(now), reason, routeID, publisherAgentID, RouteStateReady, RouteStateDraining); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE agent_route_leases SET state = ?, closed_at = ? WHERE route_id = ? AND state = ?`, RouteLeaseClosed, dbTime(now), routeID, RouteLeaseOpen); err != nil {
		return err
	}
	if err := insertRouteAuditTx(tx, now, action, "ok", groupID, routeID, "", publisherAgentID, publisherConvID, reason); err != nil {
		return err
	}
	return tx.Commit()
}

func insertRouteAuditTx(tx *sql.Tx, at time.Time, action, result string, groupID int64, routeID, leaseID, agentID, convID, detail string) error {
	_, err := tx.Exec(`INSERT INTO agent_route_audit (at, action, result, group_id, route_id, lease_id, actor_agent_id, actor_conv_id, detail) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, dbTime(at), action, result, groupID, routeID, leaseID, agentID, convID, detail)
	return err
}

func scanAgentRoute(s rowScanner) (*AgentRoute, error) {
	var route AgentRoute
	var createdAt, withdrawnAt dbTimestamp
	if err := s.Scan(&route.ID, &route.GroupID, &route.GroupName, &route.PublisherAgentID, &route.PublisherConvID, &route.PublisherLaunchGeneration, &route.GroupGeneration, &route.Name, &route.Transport, &route.Target, &route.State, &createdAt, &withdrawnAt, &route.WithdrawReason); err != nil {
		return nil, err
	}
	route.CreatedAt = createdAt.Time()
	route.WithdrawnAt = withdrawnAt.Time()
	return &route, nil
}
