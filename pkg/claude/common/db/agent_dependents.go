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
		FROM agent_cron_jobs WHERE owner_agent = ? OR target_agent = ? ORDER BY id`, agentID, agentID); err != nil {
		return nil, err
	}
	if err := read(AgentDependentStandingOrder, `SELECT o.id, o.name, o.owner_agent, COALESCE((
			SELECT GROUP_CONCAT(DISTINCT s.harness) FROM agent_standing_order_hook_selectors s WHERE s.order_id = o.id
		), '')
		FROM agent_standing_orders o WHERE o.owner_agent = ? OR o.target_agent = ? ORDER BY o.id`, agentID, agentID); err != nil {
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
func deleteAgentDependentsTx(tx *sql.Tx, agentID string) ([]AgentDependent, error) {
	deps, err := listAgentDependents(tx, agentID)
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
