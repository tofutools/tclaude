package db

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// MailboxCount is the per-conv aggregate the mailbox sidebar renders:
// how many messages a conv has received (In), sent (Out), how many
// received ones are still unread, and when its newest message (sent or
// received) landed. A conv's mailbox "total" for the dashboard is
// In+Out — the inbox+sent view a mail client presents; Last drives the
// recency sort of the mailbox list.
type MailboxCount struct {
	In     int
	Out    int
	Unread int
	Last   time.Time
}

// MailboxCounts returns per-conv message tallies across the whole
// agent_messages table in two grouped scans (received-by-to_conv and
// sent-by-from_conv), merged by conv-id. It feeds the dashboard mail
// client's mailbox list so the sidebar can show every conv that has
// any mail plus its unread badge without N per-conv queries.
//
// Unread counts only received messages the recipient has not read
// (to_conv side, read_at == ”) — mirroring an agent's own inbox
// unread. A conv appears in the map if it is the sender or recipient
// of at least one message.
func MailboxCounts() (map[string]MailboxCount, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	out := map[string]MailboxCount{}

	// bump records the newest message instant seen for a conversation.
	bump := func(conv string, maxCreated dbTimestamp) {
		c := out[conv]
		if maxCreated.Time().After(c.Last) {
			c.Last = maxCreated.Time()
		}
		out[conv] = c
	}

	// Received side: total in + unread + newest, grouped by recipient.
	inRows, err := db.Query(`SELECT to_conv,
		COUNT(*),
		SUM(CASE WHEN read_at IS NULL THEN 1 ELSE 0 END),
		MAX(created_at)
		FROM agent_messages WHERE to_conv != '' GROUP BY to_conv`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = inRows.Close() }()
	for inRows.Next() {
		var conv string
		var maxCreated dbTimestamp
		var total, unread int
		if err := inRows.Scan(&conv, &total, &unread, &maxCreated); err != nil {
			return nil, err
		}
		c := out[conv]
		c.In = total
		c.Unread = unread
		out[conv] = c
		bump(conv, maxCreated)
	}
	if err := inRows.Err(); err != nil {
		return nil, err
	}

	// Sent side: total out + newest, grouped by sender.
	outRows, err := db.Query(`SELECT from_conv, COUNT(*), MAX(created_at)
		FROM agent_messages WHERE from_conv != '' GROUP BY from_conv`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = outRows.Close() }()
	for outRows.Next() {
		var conv string
		var maxCreated dbTimestamp
		var total int
		if err := outRows.Scan(&conv, &total, &maxCreated); err != nil {
			return nil, err
		}
		c := out[conv]
		c.Out = total
		out[conv] = c
		bump(conv, maxCreated)
	}
	return out, outRows.Err()
}

// CountAgentMessages returns the number of distinct agent_messages rows
// — the badge count for the dashboard's virtual "all messages" mailbox.
// Distinct rows, not the In+Out tally MailboxCounts produces (a 1:1
// message is one row counted once here, but once as In and once as Out
// there).
func CountAgentMessages() (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM agent_messages`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// MailboxFilter selects which agent_messages rows a dashboard mailbox
// read returns — the backing query for the Messages tab's paginated,
// searchable folder view.
//
// Every predicate is a fixed-size SQL shape: sets that grow with the
// installation (retired agents, group members, title matches) are
// subqueries the database evaluates, never bound id lists, so neither the
// bind-variable limit nor a per-request Go-side resolve pass scales with
// the number of agents.
//
// ForConv scopes to one mailbox: rows where the conv is sender OR
// recipient. "" is the virtual "all" firehose (no scope). A single
// (to_conv = ? OR from_conv = ?) predicate dedups the rare self-addressed
// row for free, so the scope needs no UNION.
//
// ScopeGroupID selects a GROUP folder — the Messages-tab "view this
// group's messages" view. A row is in scope when ANY of: its sender is a
// current member, its recipient is a current member, or it is one of the
// group's own multicasts (group_id = ScopeGroupID). That OR-set is the
// group analogue of ForConv's single-conv predicate. A group folder sets
// this and leaves ForConv empty; the two are mutually-exclusive scopes.
//
// Text is the Messages-tab search box. Subject and body are matched through
// the agent_messages_fts full-text index: every word of the query must
// start a word of the message (see mailboxFTSQuery). The parties are
// matched as substrings (SQLite LIKE, ASCII case folding): from_conv /
// to_conv (so a typed conv-id fragment matches), the sender's or
// recipient's display title (see mailboxTitleConvsSQL), and the routing
// group's name.
//
// ExcludeRetired drops any row whose sender OR recipient is a retired
// agent's conv — `(sender not retired OR <handoff>) AND recipient not
// retired`. The dashboard's "all" firehose and group folders use it to
// omit retired agents' traffic unless the operator opts in; a 1:1 to/from a
// retired agent disappears, while a group broadcast (empty to_conv)
// survives unless its own sender is retired. The lone carve-out is the
// reincarnation handoff: its sender is the retired predecessor by
// construction, so the sender side relaxes to "live OR a handoff" to keep
// the live successor's birth record visible (the recipient side still must
// be live). It is an AND constraint independent of the (OR-ed) search
// predicate, so it narrows whatever the search matched.
type MailboxFilter struct {
	ForConv        string
	ScopeGroupID   int64
	Text           string
	ExcludeRetired bool
	// BeforeID is a keyset cursor: only rows older than it (id < BeforeID).
	// Zero means no cursor. Callers counting a folder's total leave it unset.
	BeforeID int64
}

// likeEscape escapes the LIKE metacharacters so the operator's search
// text matches literally — a typed '%' means a percent sign, not "any
// run". Pairs with `LIKE ? ESCAPE '\'`.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// mailboxFTSQuery turns search-box text into an FTS5 MATCH expression:
// each word (a run of letters or digits, as the unicode61 tokenizer splits
// them) becomes a quoted prefix term, and all terms must match. Quoting
// keeps operators and punctuation the operator types literal. "" when the
// text has no words.
func mailboxFTSQuery(text string) string {
	words := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	terms := make([]string, 0, len(words))
	for _, w := range words {
		terms = append(terms, `"`+w+`"*`)
	}
	return strings.Join(terms, " ")
}

// retiredConvsSQL lists the conv-ids of retired agents, matching
// ListRetiredAgents (an agent away on another federation node is not
// retired here). Blank convs are dropped.
const retiredConvsSQL = `SELECT current_conv_id FROM agents
	WHERE retired_at IS NOT NULL AND current_conv_id != ''
	AND NOT EXISTS (SELECT 1 FROM agent_federation_presence p
		WHERE p.agent_id = agents.agent_id AND p.state = 'away')`

// retiredConvExistsSQL tests one conv column (the %s) against
// retiredConvsSQL's set as a correlated lookup on the agents.current_conv_id
// unique index. A `NOT IN (retiredConvsSQL)` list would be rebuilt per
// statement — O(retired agents) for every count, which the roster runs once
// per group. An empty conv never matches (current_conv_id is never blank
// for a retired agent's lookup to hit), so multicasts with an empty to_conv
// are never excluded on that side.
const retiredConvExistsSQL = `EXISTS (SELECT 1 FROM agents ra
	WHERE ra.current_conv_id = %s AND ra.current_conv_id != '' AND ra.retired_at IS NOT NULL
	AND NOT EXISTS (SELECT 1 FROM agent_federation_presence p
		WHERE p.agent_id = ra.agent_id AND p.state = 'away'))`

// handoffIDsSQL lists the reincarnation handoff rows, served by the
// idx_agent_messages_handoff partial index so the retired-exclude carve-out
// never reads message rows (a plain `subject = ?` would).
const handoffIDsSQL = `SELECT id FROM agent_messages WHERE subject = '` + ReincarnationHandoffSubject + `'`

// groupMemberConvsSQL lists a group's current member convs (one bound
// group id), matching ListAgentGroupMembers.
const groupMemberConvsSQL = `SELECT ag.current_conv_id FROM agent_group_members m
	JOIN agents ag ON ag.agent_id = m.agent_id
	WHERE m.group_id = ? AND ag.current_conv_id != ''`

// mailboxTitleConvsSQL lists the convs whose display title matches one
// LIKE pattern (bound twice). The title follows AgentDisplayTitle's
// precedence — custom title > the owning agent's pending name > summary >
// first prompt — the same name the reading pane renders; the second arm
// covers agent convs with no conv_index row yet (pending name only).
const mailboxTitleConvsSQL = `SELECT ci.conv_id FROM conv_index ci
	LEFT JOIN agent_conversations ac ON ac.conv_id = ci.conv_id
	LEFT JOIN agents a ON a.agent_id = ac.agent_id
	WHERE COALESCE(NULLIF(ci.custom_title, ''), NULLIF(a.pending_name, ''),
		NULLIF(ci.summary, ''), ci.first_prompt) LIKE ? ESCAPE '\'
	UNION ALL
	SELECT ac.conv_id FROM agent_conversations ac
	JOIN agents a ON a.agent_id = ac.agent_id
	WHERE a.pending_name LIKE ? ESCAPE '\'
	AND NOT EXISTS (SELECT 1 FROM conv_index ci WHERE ci.conv_id = ac.conv_id)`

// where builds the WHERE clause (without the "WHERE" keyword) and its
// args. Returns ("", nil) for the unscoped, unfiltered "all" firehose so
// the caller can omit the clause entirely.
func (f MailboxFilter) where() (string, []any) {
	var clauses []string
	var args []any
	if f.ForConv != "" {
		clauses = append(clauses, "(to_conv = ? OR from_conv = ?)")
		args = append(args, f.ForConv, f.ForConv)
	}
	if f.ScopeGroupID != 0 {
		clauses = append(clauses, "(from_conv IN ("+groupMemberConvsSQL+")"+
			" OR to_conv IN ("+groupMemberConvsSQL+") OR group_id = ?)")
		args = append(args, f.ScopeGroupID, f.ScopeGroupID, f.ScopeGroupID)
	}
	if f.Text != "" {
		// One id set per statement: the arms are unioned inside a single
		// non-correlated subquery, so the title match (a conv_index pass) runs
		// once rather than per OR arm, and the outer query fetches only the
		// matching rows by id instead of scanning every message for them.
		like := "%" + likeEscape(f.Text) + "%"
		arms := []string{
			`SELECT id FROM agent_messages WHERE from_conv LIKE ? ESCAPE '\' OR to_conv LIKE ? ESCAPE '\'`,
			`SELECT m.id FROM agent_messages m JOIN titled t ON m.from_conv = t.conv_id`,
			`SELECT m.id FROM agent_messages m JOIN titled t ON m.to_conv = t.conv_id`,
			`SELECT id FROM agent_messages WHERE group_id IN (SELECT id FROM agent_groups WHERE name LIKE ? ESCAPE '\')`,
		}
		args = append(args, like, like) // mailboxTitleConvsSQL in the WITH
		args = append(args, like, like, like)
		if match := mailboxFTSQuery(f.Text); match != "" {
			arms = append(arms, `SELECT rowid FROM agent_messages_fts WHERE agent_messages_fts MATCH ?`)
			args = append(args, match)
		}
		clauses = append(clauses, "id IN (WITH titled(conv_id) AS MATERIALIZED ("+mailboxTitleConvsSQL+") "+
			strings.Join(arms, " UNION ALL ")+")")
	}
	if f.ExcludeRetired {
		clauses = append(clauses,
			"(NOT "+fmt.Sprintf(retiredConvExistsSQL, "from_conv")+" OR id IN ("+handoffIDsSQL+"))",
			"NOT "+fmt.Sprintf(retiredConvExistsSQL, "to_conv"))
	}
	if f.BeforeID > 0 {
		clauses = append(clauses, "id < ?")
		args = append(args, f.BeforeID)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return strings.Join(clauses, " AND "), args
}

// MailboxRosterTotals returns the badge totals the Messages-tab roster
// shows for its aggregate folders, all from one pass over agent_messages:
// all is the "all" firehose's row count, and groups maps each group id to its
// group folder's total (a group with no traffic is absent). excludeRetired
// applies MailboxFilter.ExcludeRetired to both. A group's total counts the
// same rows a ScopeGroupID folder lists: traffic to or from a current member
// plus the group's own multicasts, each row once.
//
// One CountMailbox per folder would re-run the exclude over the same rows
// in its own statement for every group on every 2s roster refresh; here the
// surviving rows are filtered once (over the covering
// idx_agent_messages_parties index) and fanned out to their groups.
func MailboxRosterTotals(excludeRetired bool) (all int, groups map[int64]int, err error) {
	d, err := Open()
	if err != nil {
		return 0, nil, err
	}
	live := `SELECT id, group_id, from_conv, to_conv FROM agent_messages`
	if where, _ := (MailboxFilter{ExcludeRetired: excludeRetired}).where(); where != "" {
		live += " WHERE " + where
	}
	// Group ids are positive, so the firehose total rides along as g = 0.
	rows, err := d.Query(`WITH live AS MATERIALIZED (` + live + `),
		mem AS MATERIALIZED (SELECT m.group_id AS g, ag.current_conv_id AS conv
			FROM agent_group_members m JOIN agents ag ON ag.agent_id = m.agent_id
			WHERE ag.current_conv_id != '')
		SELECT 0, COUNT(*) FROM live
		UNION ALL
		SELECT g, COUNT(DISTINCT id) FROM (
			SELECT group_id AS g, id FROM live WHERE group_id != 0
			UNION ALL SELECT mem.g, live.id FROM live JOIN mem ON mem.conv = live.from_conv
			UNION ALL SELECT mem.g, live.id FROM live JOIN mem ON mem.conv = live.to_conv
		) GROUP BY g`)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = rows.Close() }()
	groups = map[int64]int{}
	for rows.Next() {
		var g int64
		var n int
		if err := rows.Scan(&g, &n); err != nil {
			return 0, nil, err
		}
		if g == 0 {
			all = n
		} else {
			groups[g] = n
		}
	}
	return all, groups, rows.Err()
}

// sqlPlaceholders returns "?, ?, …" with n placeholders (n >= 1).
func sqlPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?, ", n-1) + "?"
}

// ListMailboxPage returns one newest-first page of the rows matching f.
// limit <= 0 means "no limit" (the whole match); offset < 0 is clamped to
// 0. Ordered by id DESC for stable newest-insertion-first pagination, including
// rows that share the same created_at instant.
func ListMailboxPage(f MailboxFilter, limit, offset int) ([]*AgentMessage, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + agentMessageColumns + `
		FROM agent_messages`
	where, args := f.where()
	if where != "" {
		q += " WHERE " + where
	}
	q += " ORDER BY id DESC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
		if offset > 0 {
			q += " OFFSET ?"
			args = append(args, offset)
		}
	}
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*AgentMessage
	for rows.Next() {
		m, err := scanAgentMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountMailbox returns how many agent_messages rows match f — the total
// the pager divides into pages. Same WHERE as ListMailboxPage, so the two
// never drift.
func CountMailbox(f MailboxFilter) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	q := `SELECT COUNT(*) FROM agent_messages`
	where, args := f.where()
	if where != "" {
		q += " WHERE " + where
	}
	var n int
	if err := d.QueryRow(q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// DeleteAgentMessagesByIDs hard-deletes the listed agent_messages rows
// unconditionally — the dashboard operator's authority (cookie + Origin)
// stands in for the per-conv party check DeleteAgentMessageByID enforces
// for an agent acting on its own mailbox. Used by the Messages tab's
// per-message and multi-select delete. A non-existent id is a silent
// skip, not an error. Returns how many rows were actually removed.
func DeleteAgentMessagesByIDs(ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	res, err := d.Exec(
		`DELETE FROM agent_messages WHERE id IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// SetAgentMessagesRead marks the listed agent_messages rows read
// (read=true, stamping read_at=now on the currently-unread ones) or unread
// (read=false, clearing read_at on the currently-read ones). It is the
// dashboard operator's authority to repair a stuck agent's inbox read-state
// — the cookie + Origin gate stands in for the per-conv party check, same as
// DeleteAgentMessagesByIDs. Marking read also acknowledges any unprocessed
// regular row, including one whose inline delivery already stamped read_at
// before a submit hook was observed. The returned count reflects either
// transition; a fully read and processed row is an idempotent no-op. A
// non-existent id is a silent skip.
// Returns how many rows changed.
//
// This is intentionally DIRECTION-AGNOSTIC: it flips read_at on whatever rows
// the ids name, regardless of whether a row was received or sent by any
// particular conv. read_at is a single shared column meaning "the recipient
// (to_conv) has read this", so marking an id the operator reached via an
// agent's *sent* messages flips that message's recipient-side read-state — the
// operator's explicit choice (the per-message reader toggle is even labelled
// "for the recipient"). This is the deliberate counterpart to
// MarkAgentMailboxRead, which is received-side-only BECAUSE a whole-folder
// "mark all read" must not silently flip other agents' read-state; do not
// "unify" the two by making this one received-only.
func SetAgentMessagesRead(ids []int64, read bool) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+2)
	// When marking read the timestamp is the first bound parameter (it
	// precedes the IN-list in the UPDATE); marking unread binds the ids only.
	if read {
		stamp := dbTime(time.Now())
		args = append(args, stamp, stamp)
	}
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	in := strings.Join(placeholders, ",")
	var q string
	if read {
		q = `UPDATE agent_messages
			SET read_at = CASE WHEN read_at IS NULL THEN ? ELSE read_at END,
				processed_at = CASE WHEN regular_send = 1 AND processed_at IS NULL THEN ? ELSE processed_at END
			WHERE (read_at IS NULL OR (regular_send = 1 AND processed_at IS NULL)) AND id IN (` + in + `)`
	} else {
		q = `UPDATE agent_messages SET read_at = NULL WHERE read_at IS NOT NULL AND id IN (` + in + `)`
	}
	res, err := d.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// MarkAgentMailboxRead marks every still-unread message a conv has RECEIVED as
// read and acknowledges any already-read, unprocessed regular row. It backs the
// dashboard's per-agent-folder "mark all read" — the operator clearing a
// stuck agent's whole inbox in one click. Only the received side is touched:
// read_at on a row the conv SENT belongs to the other party, so a folder-level
// "mark all read" must not flip the recipient's read-state. Returns how many
// rows changed.
func MarkAgentMailboxRead(conv string) (int64, error) {
	if conv == "" {
		return 0, nil
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	stamp := dbTime(time.Now())
	res, err := d.Exec(
		`UPDATE agent_messages
		 SET read_at = CASE WHEN read_at IS NULL THEN ? ELSE read_at END,
			 processed_at = CASE WHEN regular_send = 1 AND processed_at IS NULL THEN ? ELSE processed_at END
		 WHERE to_conv = ? AND (read_at IS NULL OR (regular_send = 1 AND processed_at IS NULL))`,
		stamp, stamp, conv)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// WipeAgentMessagesForConvs hard-deletes every agent_messages row where
// any of the listed convs is a party (sender or recipient) — the
// dashboard's "wipe selected mailboxes" bulk action. Because a 1:1
// message is one shared row, wiping conv A also removes A's messages
// from conv B's mailbox view; that is the intended "erase this agent's
// correspondence" semantics. Returns how many rows were removed.
func WipeAgentMessagesForConvs(convs []string) (int64, error) {
	if len(convs) == 0 {
		return 0, nil
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	placeholders := make([]string, len(convs))
	args := make([]any, 0, len(convs)*2)
	for i, c := range convs {
		placeholders[i] = "?"
		args = append(args, c)
	}
	for _, c := range convs {
		args = append(args, c)
	}
	in := strings.Join(placeholders, ",")
	res, err := d.Exec(
		`DELETE FROM agent_messages WHERE from_conv IN (`+in+`) OR to_conv IN (`+in+`)`,
		args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
