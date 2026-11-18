package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// What retiring or deleting an agent takes with it by default. The rows here
// exist only for the agent: its own cron jobs, standing orders and trigger
// rules, and the cron jobs and standing orders that address it alone. Left
// behind they either fire at nobody, record a refusal on every tick, or sit
// disabled forever (a reinstate never re-enables them). The operator can keep
// them instead; a retire then disables what it always disabled.
const (
	AgentDependentCron          = "cron"
	AgentDependentStandingOrder = "standing_order"
	AgentDependentTriggerRule   = "trigger_rule"

	// AgentDependentOwner and AgentDependentTarget say how the row refers to
	// the agent.
	AgentDependentOwner  = "owner"
	AgentDependentTarget = "target"
)

// AgentDependent is one row that refers to an agent and goes with it.
type AgentDependent struct {
	Kind string `json:"kind"`
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Via is AgentDependentOwner or AgentDependentTarget.
	Via string `json:"via"`
	// Harnesses are the native hook harnesses a standing order selects, so
	// the caller can withdraw hook declarations it no longer needs.
	Harnesses []string `json:"harnesses,omitempty"`
}

type dependentQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// ListAgentDependents returns the rows that would go with agentID, in a
// stable order: cron jobs, standing orders, trigger rules, each by id.
func ListAgentDependents(agentID string) ([]AgentDependent, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return listAgentDependents(d, agentID)
}

func listAgentDependents(q dependentQuerier, agentID string) ([]AgentDependent, error) {
	return listAgentDependentsVia(q, agentID, false)
}

// listAgentDependentsVia is listAgentDependents, limited to the rows the
// agent owns when ownedOnly is set.
func listAgentDependentsVia(q dependentQuerier, agentID string, ownedOnly bool) ([]AgentDependent, error) {
	addressed := "OR target_agent = ?"
	if ownedOnly {
		addressed = "AND ? <> ''"
	}
	var out []AgentDependent
	read := func(kind, query string, args ...any) error {
		rows, err := q.Query(query, args...)
		if err != nil {
			return fmt.Errorf("list %s dependents: %w", kind, err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			dep := AgentDependent{Kind: kind}
			var owner, harnesses string
			if err := rows.Scan(&dep.ID, &dep.Name, &owner, &harnesses); err != nil {
				return fmt.Errorf("list %s dependents: %w", kind, err)
			}
			dep.Via = AgentDependentTarget
			if owner == agentID {
				dep.Via = AgentDependentOwner
			}
			for _, h := range strings.Split(harnesses, ",") {
				if h = strings.TrimSpace(h); h != "" {
					dep.Harnesses = append(dep.Harnesses, h)
				}
			}
			out = append(out, dep)
		}
		return rows.Err()
	}
	if err := read(AgentDependentCron, `SELECT id, name, owner_agent, ''
		FROM agent_cron_jobs WHERE owner_agent = ? `+addressed+` ORDER BY id`, agentID, agentID); err != nil {
		return nil, err
	}
	if err := read(AgentDependentStandingOrder, `SELECT o.id, o.name, o.owner_agent, COALESCE((
			SELECT GROUP_CONCAT(DISTINCT s.harness) FROM agent_standing_order_hook_selectors s WHERE s.order_id = o.id
		), '')
		FROM agent_standing_orders o WHERE o.owner_agent = ? `+strings.ReplaceAll(addressed, "target_agent", "o.target_agent")+` ORDER BY o.id`, agentID, agentID); err != nil {
		return nil, err
	}
	if err := read(AgentDependentTriggerRule, `SELECT id, name, owner_agent, ''
		FROM trigger_rules WHERE owner_agent = ? ORDER BY id`, agentID); err != nil {
		return nil, err
	}
	return out, nil
}

// deleteAgentDependentsTx removes every row ListAgentDependents names, with
// the ledgers that only mean something for them, and returns what it removed.
func deleteAgentDependentsTx(tx *sql.Tx, agentID string, ownedOnly bool) ([]AgentDependent, error) {
	deps, err := listAgentDependentsVia(tx, agentID, ownedOnly)
	if err != nil {
		return nil, err
	}
	for _, dep := range deps {
		var stmts []string
		switch dep.Kind {
		case AgentDependentCron:
			// agent_cron_runs cascade through their FK.
			stmts = []string{`DELETE FROM agent_cron_jobs WHERE id = ?`}
		case AgentDependentStandingOrder:
			// Debounce rows, scopes and selectors cascade; the delivery ledger
			// is removed by hand, as DeleteStandingOrder does.
			stmts = []string{
				`DELETE FROM agent_standing_order_deliveries WHERE order_id = ?`,
				`DELETE FROM agent_standing_orders WHERE id = ?`,
			}
		case AgentDependentTriggerRule:
			// Firings and workers keep their history (rule_id SET NULL).
			stmts = []string{
				`DELETE FROM trigger_dwell_states WHERE rule_id = ?`,
				`DELETE FROM trigger_rules WHERE id = ?`,
			}
		}
		for _, stmt := range stmts {
			if _, err := tx.Exec(stmt, dep.ID); err != nil {
				return nil, fmt.Errorf("remove %s %d: %w", dep.Kind, dep.ID, err)
			}
		}
	}
	return deps, nil
}

// AgentScopedRun is a run that is not over and is scoped to one agent.
type AgentScopedRun struct {
	ID          string `json:"id"`
	TemplateRef string `json:"template_ref"`
}

// ListAgentScopedActiveRuns returns the runs that are not over and are
// scoped to agentID, bound or already orphaned, oldest first. Its retirement
// or deletion leaves them nothing to run on.
func ListAgentScopedActiveRuns(agentID string) ([]AgentScopedRun, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id, template_ref FROM process_runs
		WHERE scope_kind = 'agent' AND scope_agent_id = ?
		  AND status NOT IN ('completed', 'failed', 'canceled')
		ORDER BY created_at, id`, agentID)
	if err != nil {
		return nil, fmt.Errorf("list agent-scoped runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AgentScopedRun
	for rows.Next() {
		var run AgentScopedRun
		if err := rows.Scan(&run.ID, &run.TemplateRef); err != nil {
			return nil, fmt.Errorf("list agent-scoped runs: %w", err)
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// Orphaned agent states: what became of an agent that rows or runs still
// refer to.
const (
	OrphanedAgentRetired = "retired"
	OrphanedAgentDeleted = "deleted"
)

// OrphanedAgent is a retired or deleted agent that cron jobs, standing
// orders, trigger rules or unfinished runs still refer to. Retirements and
// deletions before the cascade, and those that kept their dependents, leave
// these behind.
type OrphanedAgent struct {
	AgentID string `json:"agent_id"`
	// State is OrphanedAgentRetired or OrphanedAgentDeleted.
	State string `json:"state"`
	// ConvID is a retired agent's last conversation; empty once deleted.
	ConvID string `json:"conv_id,omitempty"`
}

// ListOrphanedAgents returns every retired or deleted agent that something
// listed by ListAgentDependents, or an unfinished run scoped to it, still
// refers to, by agent id. An agent away on a federation peer is not one.
func ListOrphanedAgents() ([]OrphanedAgent, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`WITH refs(ref) AS (
			SELECT owner_agent FROM agent_cron_jobs
			UNION SELECT target_agent FROM agent_cron_jobs
			UNION SELECT owner_agent FROM agent_standing_orders
			UNION SELECT target_agent FROM agent_standing_orders
			UNION SELECT owner_agent FROM trigger_rules
			UNION SELECT scope_agent_id FROM process_runs
				WHERE scope_kind = 'agent' AND status NOT IN ('completed', 'failed', 'canceled')
		)
		SELECT r.ref, COALESCE(a.current_conv_id, ''), a.agent_id IS NOT NULL
		FROM refs r LEFT JOIN agents a ON a.agent_id = r.ref
		WHERE COALESCE(r.ref, '') <> '' AND (a.agent_id IS NULL OR a.retired_at IS NOT NULL)
		  AND `+notFederatedAway("r.ref")+`
		ORDER BY r.ref`)
	if err != nil {
		return nil, fmt.Errorf("list orphaned agents: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []OrphanedAgent
	for rows.Next() {
		var o OrphanedAgent
		var exists bool
		if err := rows.Scan(&o.AgentID, &o.ConvID, &exists); err != nil {
			return nil, fmt.Errorf("list orphaned agents: %w", err)
		}
		o.State = OrphanedAgentDeleted
		if exists {
			o.State = OrphanedAgentRetired
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// federatedAwayStates are the federation presence states of an agent that
// lives on, elsewhere: away on a peer, reserved for one, or ended there. Its
// home row is retired (system:federation-away) but it is not gone, and an
// ordinary reinstate refuses it for the same reason.
const federatedAwayStates = `('away', 'reserved', 'terminal')`

func notFederatedAway(agentRef string) string {
	return `NOT EXISTS (SELECT 1 FROM agent_federation_presence p WHERE p.agent_id = ` + agentRef +
		` AND p.state IN ` + federatedAwayStates + `)`
}

// RemoveOrphanedAgentDependents removes what ListAgentDependents names for
// agentID, but only while the agent is still retired or deleted, and not
// away on a federation peer: an agent reinstated meanwhile keeps everything,
// and orphaned is false.
func RemoveOrphanedAgentDependents(agentID string) (removed []AgentDependent, orphaned bool, err error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, false, nil
	}
	d, err := Open()
	if err != nil {
		return nil, false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var live int
	if err := tx.QueryRow(`SELECT (SELECT COUNT(*) FROM agents WHERE agent_id = ?1 AND retired_at IS NULL)
		+ (SELECT COUNT(*) FROM agent_federation_presence WHERE agent_id = ?1 AND state IN `+federatedAwayStates+`)`,
		agentID).Scan(&live); err != nil {
		return nil, false, fmt.Errorf("check agent %s: %w", agentID, err)
	}
	if live > 0 {
		return nil, false, nil
	}
	if removed, err = deleteAgentDependentsTx(tx, agentID, false); err != nil {
		return nil, true, err
	}
	if err := tx.Commit(); err != nil {
		return nil, true, err
	}
	return removed, true, nil
}
