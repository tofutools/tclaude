package db

import "fmt"

// agentForConvExpr is the SQL VALUES expression that resolves a bound conv-id to
// its owning agent_id via agent_conversations (conv_id is its PK, so at most one
// row), or '' when the conv is not an actor / unmapped. It is the insert-time
// dual-write counterpart of the v77 backfill (backfillAgentColSQL): a freshly
// written row carries the same agent companion the migration computed, so
// backfilled and new rows agree. Bind the conv-id once per occurrence, in
// VALUES order. See v77AgentColumns for the columns this feeds.
const agentForConvExpr = `COALESCE((SELECT agent_id FROM agent_conversations WHERE conv_id = ?), '')`

// agentForSuccessionExpr resolves a succession edge to its (single) actor. Both
// endpoints are generations of the SAME agent, so either resolves it; the
// successor (new) is tried first, falling back to the predecessor (old). The
// predecessor is always already enrolled when an edge is written (it is the live
// head being succeeded), so this resolves at write time even though the
// successor may not have linked yet. Bind new conv then old conv.
const agentForSuccessionExpr = `COALESCE((SELECT agent_id FROM agent_conversations WHERE conv_id = ?), (SELECT agent_id FROM agent_conversations WHERE conv_id = ?), '')`

// propagateAgentCompanions fills the sessions.agent_id companion for a conv that
// has just been linked to an agent. A session row is registered before the hook
// enrolls the agent, so its agent_id derives to '' at insert time — this fills
// it on enrollment. (Other owner tables resolve at insert because their conv is
// already enrolled by the time the row is written; succession edges resolve via
// their predecessor, which is always enrolled — see agentForSuccessionExpr.)
//
// Idempotent and best-effort: it only fills rows still '' for this conv, a no-op
// once populated. Runs inside the caller's enrollment transaction (linkConvTx /
// advanceAgentToNewConv) so it never partially commits.
func propagateAgentCompanions(x dbExecQuerier, convID, agentID string) error {
	if convID == "" || agentID == "" {
		return nil
	}
	// linkConvTx / advanceAgentToNewConv also run during the v72 backfill, before
	// the v77 sessions.agent_id column exists — and on a partial-schema heal DB
	// the sessions table may be absent entirely. Probe first and skip when the
	// column isn't there yet; the v77 backfill populates existing rows when it
	// finally runs.
	var has int
	if err := x.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'agent_id'`).Scan(&has); err != nil {
		return fmt.Errorf("propagate agent companion (probe sessions.agent_id): %w", err)
	}
	if has == 0 {
		return nil
	}
	// Column/table names are caller-hardcoded, never user input.
	if _, err := x.Exec(
		`UPDATE sessions SET agent_id = ? WHERE conv_id = ? AND agent_id = ''`,
		agentID, convID); err != nil {
		return fmt.Errorf("propagate agent companion sessions.agent_id: %w", err)
	}
	return nil
}
