package db

import (
	"strings"
	"time"
)

// Paginated, SQL-side listers + counts behind the dashboard's three
// formerly-unbounded snapshot lists — retired agents, non-agent
// conversations, and replaced (superseded) generations. The 2s
// /api/snapshot poll used to embed each list in full; these power the
// dedicated /api/retired, /api/conversations and /api/replaced endpoints
// that page + filter server-side instead, so a poll never ships hundreds
// of rows. Each pair shares a shape: a `*Page(q, offset, limit)` lister
// (limit <= 0 == UNBOUNDED, no LIMIT) and a `Count*(q)` returning
// (total-matching-q, total-ignoring-q) so the handler can render the pager
// and the "n / N" search chip without a second pass. All filtering + windowing
// is in SQL — never scan-all-then-slice-in-Go.

// dashListLike turns a non-empty query into a case-insensitive contains
// pattern. Pairs with `LOWER(col) LIKE ? ESCAPE '\'` so a typed % / _ in q
// matches literally (likeEscape) rather than acting as a wildcard.
func dashListLike(q string) string {
	return "%" + likeEscape(strings.ToLower(q)) + "%"
}

// --- Retired agents (GET /api/retired) ------------------------------------

// retiredAgentCols is the agents column list in the exact order scanAgent
// expects, qualified to the `a` alias so it composes with the conv_index
// LEFT JOIN the q filter adds.
const retiredAgentCols = `a.agent_id, a.current_conv_id, a.created_at, a.created_via,
	a.retired_at, a.retired_by, a.retire_reason, a.pending_name, a.retired_by_agent`

// retiredAgentQ builds the optional q predicate (an AND-fragment) + args for
// the retired listing. The "title" match spans the conv_index columns
// agent.CachedTitle resolves through (custom_title / summary / first_prompt)
// plus the actor's pending_name, LEFT-JOINed on the actor's live conv; conv-id
// and agent-id match the agents columns directly. "" q => ("", nil) so the
// caller omits the join entirely.
func retiredAgentQ(q string) (string, []any) {
	if q == "" {
		return "", nil
	}
	like := dashListLike(q)
	clause := ` AND (LOWER(a.agent_id) LIKE ? ESCAPE '\'
		OR LOWER(a.current_conv_id) LIKE ? ESCAPE '\'
		OR LOWER(a.pending_name) LIKE ? ESCAPE '\'
		OR LOWER(COALESCE(ci.custom_title, '')) LIKE ? ESCAPE '\'
		OR LOWER(COALESCE(ci.summary, '')) LIKE ? ESCAPE '\'
		OR LOWER(COALESCE(ci.first_prompt, '')) LIKE ? ESCAPE '\')`
	return clause, []any{like, like, like, like, like, like}
}

// ListRetiredAgentsPage returns one newest-retirement-first page of retired
// actors (agents.retired_at != ”), filtered by q and windowed by offset/limit.
// limit <= 0 returns the whole (q-filtered) set with no LIMIT — the modal
// "show all" path. The conv_index LEFT JOIN (PK conv_id, so 1:1) only exists to
// let the title columns participate in the q filter; it never multiplies rows.
func ListRetiredAgentsPage(q string, offset, limit int) ([]*Agent, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	qClause, args := retiredAgentQ(q)
	sql := `SELECT ` + retiredAgentCols + `
		FROM agents a
		LEFT JOIN conv_index ci ON ci.conv_id = a.current_conv_id
		WHERE a.retired_at != ''` + qClause + `
		ORDER BY a.retired_at DESC, a.agent_id`
	if limit > 0 {
		sql += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := d.Query(sql, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Agent
	for rows.Next() {
		a, serr := scanAgent(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountRetiredAgents returns (total matching q, total ignoring q) over the
// retired actors. totalUnfiltered is the cheap join-free count; total adds the
// conv_index join only when q is set.
func CountRetiredAgents(q string) (total, totalUnfiltered int, err error) {
	d, err := Open()
	if err != nil {
		return 0, 0, err
	}
	if err = d.QueryRow(`SELECT COUNT(*) FROM agents WHERE retired_at != ''`).Scan(&totalUnfiltered); err != nil {
		return 0, 0, err
	}
	if q == "" {
		return totalUnfiltered, totalUnfiltered, nil
	}
	qClause, args := retiredAgentQ(q)
	sql := `SELECT COUNT(*) FROM agents a
		LEFT JOIN conv_index ci ON ci.conv_id = a.current_conv_id
		WHERE a.retired_at != ''` + qClause
	if err = d.QueryRow(sql, args...).Scan(&total); err != nil {
		return 0, 0, err
	}
	return total, totalUnfiltered, nil
}

// --- Non-agent conversations (GET /api/conversations) ---------------------

// nonAgentConvBase is the shared FROM + WHERE for the promotion-candidate
// listing: live (non-sidechain, non-archived) conv_index rows that are NOT a
// generation of any agent (the SQL form of the old ListAgentConvIDs exclusion).
const nonAgentConvBase = `FROM conv_index
	WHERE is_sidechain = 0 AND archived_at = ''
	  AND conv_id NOT IN (SELECT conv_id FROM agent_conversations)`

// nonAgentConvQ builds the optional q predicate (title / conv-id LIKE) for the
// conversations listing. These rows are non-agents, so there is no agent_id to
// match — title spans the FormatConvTitle source columns.
func nonAgentConvQ(q string) (string, []any) {
	if q == "" {
		return "", nil
	}
	like := dashListLike(q)
	clause := ` AND (LOWER(conv_id) LIKE ? ESCAPE '\'
		OR LOWER(COALESCE(custom_title, '')) LIKE ? ESCAPE '\'
		OR LOWER(COALESCE(summary, '')) LIKE ? ESCAPE '\'
		OR LOWER(COALESCE(first_prompt, '')) LIKE ? ESCAPE '\')`
	return clause, []any{like, like, like, like}
}

// ListNonAgentConvIndexPage returns one newest-modified-first page of the
// promotion-candidate conversations (non-agent conv_index rows), filtered by q
// and windowed by offset/limit (limit <= 0 == no LIMIT). Reuses the
// ListRecentConvIndex column list + scanConvIndexRows.
func ListNonAgentConvIndexPage(q string, offset, limit int) ([]*ConvIndexRow, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	qClause, args := nonAgentConvQ(q)
	sql := `SELECT conv_id, project_dir, full_path, file_mtime, file_size,
		first_prompt, summary, custom_title, message_count,
		created, modified, git_branch, project_path, is_sidechain, indexed_at,
		archived_at, git_branch_startup, harness
		` + nonAgentConvBase + qClause + `
		ORDER BY file_mtime DESC`
	if limit > 0 {
		sql += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := d.Query(sql, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanConvIndexRows(rows)
}

// CountNonAgentConvIndex returns (total matching q, total ignoring q) over the
// promotion-candidate conversations.
func CountNonAgentConvIndex(q string) (total, totalUnfiltered int, err error) {
	d, err := Open()
	if err != nil {
		return 0, 0, err
	}
	if err = d.QueryRow(`SELECT COUNT(*) ` + nonAgentConvBase).Scan(&totalUnfiltered); err != nil {
		return 0, 0, err
	}
	if q == "" {
		return totalUnfiltered, totalUnfiltered, nil
	}
	qClause, args := nonAgentConvQ(q)
	if err = d.QueryRow(`SELECT COUNT(*) `+nonAgentConvBase+qClause, args...).Scan(&total); err != nil {
		return 0, 0, err
	}
	return total, totalUnfiltered, nil
}

// --- Replaced generations (GET /api/replaced) -----------------------------

// ReplacedGenerationRow is one superseded predecessor generation joined to its
// owning actor — the SQL form of the dashboard's old O(actors) GenerationsForAgent
// walk. The JOIN to agents drops orphan successions (actor deleted) and the
// old_conv_id <> current_conv_id guard drops the live head, exactly matching the
// old walk's row set. Title resolution stays in the agentd handler.
type ReplacedGenerationRow struct {
	OldConvID     string    // the predecessor generation's conv-id
	Reason        string    // how it was superseded (the succession edge's reason)
	SucceededAt   time.Time // when it was superseded
	AgentID       string    // the owning actor
	CurrentConvID string    // the actor's live head generation
	RetiredAt     string    // the actor's raw retired_at ("" == active)
}

// replacedGenQ builds the optional q predicate for the replaced listing. The
// join fragment adds conv_index (on the predecessor) only when q is set, so the
// "title" match can span the predecessor's title columns; conv-id matches
// old_conv_id and agent-id matches s.agent_id directly.
func replacedGenQ(q string) (joinFrag, whereFrag string, args []any) {
	if q == "" {
		return "", "", nil
	}
	like := dashListLike(q)
	joinFrag = ` LEFT JOIN conv_index ci ON ci.conv_id = s.old_conv_id`
	whereFrag = ` AND (LOWER(s.old_conv_id) LIKE ? ESCAPE '\'
		OR LOWER(s.agent_id) LIKE ? ESCAPE '\'
		OR LOWER(COALESCE(ci.custom_title, '')) LIKE ? ESCAPE '\'
		OR LOWER(COALESCE(ci.summary, '')) LIKE ? ESCAPE '\'
		OR LOWER(COALESCE(ci.first_prompt, '')) LIKE ? ESCAPE '\')`
	return joinFrag, whereFrag, []any{like, like, like, like, like}
}

// ListReplacedGenerationsPage returns one newest-replacement-first page of
// superseded predecessor generations, each joined to its still-existing actor,
// filtered by q and windowed by offset/limit (limit <= 0 == no LIMIT). It
// REPLACES the old per-actor GenerationsForAgent walk: the JOIN to agents drops
// orphan successions (deleted actor) and the old_conv_id <> current_conv_id
// guard drops the live head, so the row set equals the old walk's predecessors.
func ListReplacedGenerationsPage(q string, offset, limit int) ([]*ReplacedGenerationRow, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	joinFrag, whereFrag, args := replacedGenQ(q)
	sql := `SELECT s.old_conv_id, s.reason, s.succeeded_at, s.agent_id, a.current_conv_id, a.retired_at
		FROM agent_conv_succession s
		JOIN agents a ON a.agent_id = s.agent_id` + joinFrag + `
		WHERE s.old_conv_id <> a.current_conv_id` + whereFrag + `
		ORDER BY s.succeeded_at DESC, s.rowid DESC`
	if limit > 0 {
		sql += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := d.Query(sql, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*ReplacedGenerationRow
	for rows.Next() {
		var (
			r     ReplacedGenerationRow
			tsRaw string
		)
		if err := rows.Scan(&r.OldConvID, &r.Reason, &tsRaw, &r.AgentID, &r.CurrentConvID, &r.RetiredAt); err != nil {
			return nil, err
		}
		r.SucceededAt, _ = time.Parse(time.RFC3339, tsRaw)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// CountReplacedGenerations returns (total matching q, total ignoring q) over the
// replaced generations.
func CountReplacedGenerations(q string) (total, totalUnfiltered int, err error) {
	d, err := Open()
	if err != nil {
		return 0, 0, err
	}
	if err = d.QueryRow(`SELECT COUNT(*) FROM agent_conv_succession s
		JOIN agents a ON a.agent_id = s.agent_id
		WHERE s.old_conv_id <> a.current_conv_id`).Scan(&totalUnfiltered); err != nil {
		return 0, 0, err
	}
	if q == "" {
		return totalUnfiltered, totalUnfiltered, nil
	}
	joinFrag, whereFrag, args := replacedGenQ(q)
	sql := `SELECT COUNT(*) FROM agent_conv_succession s
		JOIN agents a ON a.agent_id = s.agent_id` + joinFrag + `
		WHERE s.old_conv_id <> a.current_conv_id` + whereFrag
	if err = d.QueryRow(sql, args...).Scan(&total); err != nil {
		return 0, 0, err
	}
	return total, totalUnfiltered, nil
}
