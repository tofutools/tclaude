package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// InsertAgentMessage records a message and returns its ID. A GroupID of
// 0 means "direct" — a message with no routing group, the universal-
// inbox transport for solo agents and cross-group sends. Any positive
// GroupID is the group that authorised a group-routed send.
func InsertAgentMessage(m *AgentMessage) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	if m.OperatorAuthored {
		tx, err := db.Begin()
		if err != nil {
			return 0, err
		}
		defer func() { _ = tx.Rollback() }()
		id, err := insertAgentMessage(tx, m)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`INSERT INTO operator_agent_messages (message_id) VALUES (?)`, id); err != nil {
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return id, nil
	}
	return insertAgentMessage(db, m)
}

// AgentMessageQueueFullError reports that a bounded regular-message insert
// found the target's durable regular-message backlog at capacity. Pending excludes the
// rejected message; no row is written when this error is returned.
type AgentMessageQueueFullError struct {
	Pending int
	Limit   int
}

func (e *AgentMessageQueueFullError) Error() string {
	return fmt.Sprintf("agent message backlog is full (%d/%d unprocessed)", e.Pending, e.Limit)
}

// InsertAgentMessageBounded atomically checks the target's durable regular
// backlog and inserts m only when fewer than limit messages are unprocessed.
// The returned pending depth includes the newly inserted message. Internal
// lifecycle and scheduler messages deliberately keep using InsertAgentMessage,
// so they cannot be dropped or blocked by regular-message backpressure.
func InsertAgentMessageBounded(m *AgentMessage, limit int) (id int64, pending int, err error) {
	if limit <= 0 {
		return 0, 0, fmt.Errorf("agent message queue limit must be positive")
	}
	m.RegularSend = true
	return insertAgentMessageWithAttachmentsBounded(m, nil, limit)
}

func insertAgentMessage(db dbExecQuerier, m *AgentMessage) (int64, error) {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	// Dual-write the stable actor refs (JOH-27 PR3a): from_agent / to_agent are
	// DERIVED from from_conv / to_conv via agent_conversations (conv_id is its
	// PK, so each correlated subquery returns at most one row). COALESCE(...,'')
	// keeps a non-actor conv — a plain conv, or one with no actor row yet — as ''
	// rather than NULL. This is the same join migrateV75toV76 backfilled with, so
	// existing and freshly-inserted rows agree. Any FromAgent/ToAgent preset on
	// the struct is intentionally ignored: the conv columns are the source of
	// truth, so the denormalised actor refs can never drift from them.
	// The audience-agent companions (JOH-284) are DERIVED here too — the same
	// boundary, the same agent_conversations resolution as from_agent/to_agent
	// — but per-element over the conv arrays, which a scalar SQL subquery can't
	// express, so they're computed in Go and passed as JSON. Any
	// To/CcRecipientAgents preset on the struct is intentionally ignored (the
	// conv arrays are the source of truth, mirroring the from/to_agent rule).
	// pin_gen is explicit caller intent (prev-gen targeting, JOH-310), NOT
	// derived from the conv columns like from_agent/to_agent — stored 1/0.
	pinGen := 0
	if m.PinGen {
		pinGen = 1
	}
	regularSend := 0
	if m.RegularSend {
		regularSend = 1
	}
	res, err := db.Exec(`INSERT INTO agent_messages
		(group_id, from_conv, to_conv, from_agent, to_agent, subject, body, parent_id,
		 created_at, delivered_at, read_at,
		 to_recipients, cc_recipients, to_recipient_agents, cc_recipient_agents, original_to_conv, pin_gen,
		 regular_send, started_at, processed_at, nudge_discarded_at)
		VALUES (?, ?, ?,
		 COALESCE((SELECT agent_id FROM agent_conversations WHERE conv_id = ?), ''),
		 COALESCE((SELECT agent_id FROM agent_conversations WHERE conv_id = ?), ''),
		 ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.GroupID, m.FromConv, m.ToConv, m.FromConv, m.ToConv, m.Subject, m.Body, m.ParentID,
		dbTime(m.CreatedAt),
		formatTimeOrEmpty(m.DeliveredAt), formatTimeOrEmpty(m.ReadAt),
		recipientsToJSON(m.ToRecipients), recipientsToJSON(m.CcRecipients),
		recipientAgentsJSON(db, m.ToRecipients), recipientAgentsJSON(db, m.CcRecipients),
		m.OriginalToConv, pinGen, regularSend,
		formatTimeOrEmpty(m.StartedAt), formatTimeOrEmpty(m.ProcessedAt), formatTimeOrEmpty(m.NudgeDiscardedAt))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// PruneAgentMessagesForConv deletes agent_messages rows older than
// olderThan that this conv is either the sender or recipient of.
// When readOnly is true, only rows the recipient has read are
// deleted (i.e. read_at is non-empty in the row's stored format).
// Returns the number of rows removed.
func PruneAgentMessagesForConv(forConv string, olderThan time.Time, readOnly bool) (int64, error) {
	if forConv == "" {
		return 0, fmt.Errorf("forConv required")
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	cutoff := dbTime(olderThan)
	q := `DELETE FROM agent_messages
		WHERE (from_conv = ? OR to_conv = ?)
		  AND created_at < ?`
	args := []any{forConv, forConv, cutoff}
	if readOnly {
		q += ` AND read_at IS NOT NULL`
	}
	res, err := d.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PruneAgentMessagesForActor is the actor-keyed twin of
// PruneAgentMessagesForConv: it prunes the rows the caller is a party to by
// current conv (from_conv/to_conv) OR stable actor (from_agent/to_agent), so
// an agent's `inbox prune` reaps mail from ALL its generations — including
// ones it received before a reincarnate / /clear (JOH-317) — rather than only
// the current conv's slice, while still reaping current-conv rows whose agent
// companion is ” (a conv messaged before it enrolled). readOnly still
// restricts to messages the recipient has read. Whichever of conv/agentID is
// empty is skipped (see actorMatchClause) rather than emitted as `col = ”`,
// which would over-match and reap unrelated non-actor / bookkeeping rows.
func PruneAgentMessagesForActor(conv, agentID string, olderThan time.Time, readOnly bool) (int64, error) {
	where, args := actorMatchClause(
		[2]string{"from_conv", conv}, [2]string{"to_conv", conv},
		[2]string{"from_agent", agentID}, [2]string{"to_agent", agentID})
	if where == "" {
		return 0, fmt.Errorf("conv or agentID required")
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	q := `DELETE FROM agent_messages WHERE ` + where + ` AND created_at < ?`
	args = append(args, dbTime(olderThan))
	if readOnly {
		q += ` AND read_at IS NOT NULL`
	}
	res, err := d.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// MarkAgentMessageDelivered sets delivered_at = now for the given message ID.
func MarkAgentMessageDelivered(id int64) error {
	return MarkAgentMessageDeliveredState(id, false)
}

// MarkAgentMessageDeliveredState records direct delivery. Inline delivery also
// marks the archival inbox copy read in the same write, so a crash cannot leave
// an unread reminder for content already injected into the pane.
func MarkAgentMessageDeliveredState(id int64, consumed bool) error {
	db, err := Open()
	if err != nil {
		return err
	}
	now := dbTime(time.Now())
	var readAt any
	if consumed {
		readAt = now
	}
	_, err = db.Exec(`UPDATE agent_messages
		SET delivered_at = ?, read_at = CASE WHEN ? IS NOT NULL THEN ? ELSE read_at END, nudge_claimed_at = NULL
		WHERE id = ?`, now, readAt, readAt, id)
	return err
}

// RederiveAgentMessageActorRefs re-runs the from_agent / to_agent dual-write
// for one message against the CURRENT agent_conversations mapping. The derived
// columns are stamped at insert (see insertAgentMessage), so a row written
// while its recipient conv was not yet linked to an actor lands with to_agent =
// ” and drops out of the actor-keyed inbox once that actor rotates again.
//
// Reincarnation's launch-enrollment path is exactly that case (TCL-731): the
// successor's handoff row must exist BEFORE the fork — the launch prompt names
// its message id — but the successor conv is only linked onto the predecessor's
// actor by RotateAgentConv afterwards. Calling this once the rotation has
// committed re-derives the companions from the same subquery the INSERT used,
// so the conv columns stay the single source of truth.
//
// Only empty companions are filled: a row that already resolved keeps what it
// has, so a later conv rotation can never rewrite settled history. Idempotent.
func RederiveAgentMessageActorRefs(id int64) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE agent_messages SET
		from_agent = CASE WHEN from_agent = '' THEN
			COALESCE((SELECT agent_id FROM agent_conversations WHERE conv_id = agent_messages.from_conv), '')
			ELSE from_agent END,
		to_agent = CASE WHEN to_agent = '' THEN
			COALESCE((SELECT agent_id FROM agent_conversations WHERE conv_id = agent_messages.to_conv), '')
			ELSE to_agent END
		WHERE id = ?`, id)
	return err
}

// ListUndeliveredAgentMessagesFor returns messages addressed to toConv
// whose delivered_at is still empty, oldest first. Used by the flush-
// on-online path so messages queued while the recipient was offline
// get nudged when they come back.
//
// Ordering is by id (autoincrement = insertion order), NOT created_at.
// created_at is stored as an RFC3339Nano string and compared lexically by
// SQLite; a time that lands exactly on a whole second serialises with no
// fractional part ("…:00Z") and sorts AFTER a later same-second value
// ("…:00.004Z") because '.' < 'Z'. ORDER BY created_at could therefore put a
// newer message before an older one — the macOS-CI flake behind
// TestListUndeliveredAgentMessagesFor. id is monotonic with insertion, giving
// a correct, total oldest-first order independent of the timestamp format.
func ListUndeliveredAgentMessagesFor(toConv string) ([]*AgentMessage, error) {
	if toConv == "" {
		return nil, nil
	}
	db, err := Open()
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + agentMessageColumns + `
		FROM agent_messages
		WHERE to_conv = ? AND delivered_at IS NULL AND read_at IS NULL AND nudge_cancelled_at IS NULL
		ORDER BY id ASC`
	return queryRows(db, scanAgentMessage, q, toConv)
}

// ListUndeliveredForAgent returns undelivered, head-following messages for an
// agent — to_agent matches AND pin_gen = 0 — oldest first (by id, the same
// insertion-order reason ListUndeliveredAgentMessagesFor documents). This is
// the agent-keyed delivery queue (JOH-310): keyed on the stable agent_id and
// EXCLUDING prev-gen-pinned rows (pin_gen=1, which stick to their exact conv
// and are drained by ListUndeliveredForExactConv). Because it keys on the
// actor, a message queued before the recipient reincarnated / ran /clear is
// still found and delivered to the agent's current head generation.
func ListUndeliveredForAgent(agentID string) ([]*AgentMessage, error) {
	if agentID == "" {
		return nil, nil
	}
	db, err := Open()
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + agentMessageColumns + `
		FROM agent_messages
		WHERE to_agent = ? AND pin_gen = 0 AND delivered_at IS NULL AND read_at IS NULL AND nudge_cancelled_at IS NULL
		ORDER BY id ASC`
	return queryRows(db, scanAgentMessage, q, agentID)
}

// ListUndeliveredForExactConv returns undelivered messages addressed to a
// SPECIFIC conv that must NOT follow an agent's head — prev-gen-pinned rows
// (pin_gen=1) and messages to a non-actor conv (to_agent=”) — oldest first.
// It is the complement of ListUndeliveredForAgent: together they partition the
// undelivered set with no overlap, so the agent-keyed and conv-keyed drains
// never double-target one message. Head-following agent messages (pin_gen=0,
// to_agent!=”) are deliberately excluded here — they belong to the agent
// drain, which delivers them to the live generation.
func ListUndeliveredForExactConv(convID string) ([]*AgentMessage, error) {
	if convID == "" {
		return nil, nil
	}
	db, err := Open()
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + agentMessageColumns + `
		FROM agent_messages
		WHERE to_conv = ? AND (pin_gen = 1 OR to_agent = '') AND delivered_at IS NULL AND read_at IS NULL AND nudge_cancelled_at IS NULL
		ORDER BY id ASC`
	return queryRows(db, scanAgentMessage, q, convID)
}

// CountUndeliveredForAgent returns how many head-following messages are queued
// for an agent (the predicate ListUndeliveredForAgent walks). It is the
// "queue depth" the send response reports back to the sender (JOH-310) so the
// caller sees how deep the recipient's inbox-nudge queue is without blocking on
// delivery.
func CountUndeliveredForAgent(agentID string) (int, error) {
	if agentID == "" {
		return 0, nil
	}
	db, err := Open()
	if err != nil {
		return 0, err
	}
	var n int
	err = db.QueryRow(
		`SELECT COUNT(*) FROM agent_messages WHERE to_agent = ? AND pin_gen = 0 AND delivered_at IS NULL AND read_at IS NULL AND nudge_cancelled_at IS NULL`,
		agentID).Scan(&n)
	return n, err
}

// CountUndeliveredForExactConv is the conv-keyed twin of CountUndeliveredForAgent
// (same predicate as ListUndeliveredForExactConv): the queue depth reported for
// a prev-gen-pinned or non-actor-conv send.
func CountUndeliveredForExactConv(convID string) (int, error) {
	if convID == "" {
		return 0, nil
	}
	db, err := Open()
	if err != nil {
		return 0, err
	}
	var n int
	err = db.QueryRow(
		`SELECT COUNT(*) FROM agent_messages WHERE to_conv = ? AND (pin_gen = 1 OR to_agent = '') AND delivered_at IS NULL AND read_at IS NULL AND nudge_cancelled_at IS NULL`,
		convID).Scan(&n)
	return n, err
}

// ListAllUndeliveredAgentMessages returns every unread message whose first
// nudge has not completed, in insertion order. It feeds the daemon's periodic
// stale-queue health scan; delivery itself stays partitioned by agent/conv via
// the narrower list functions above.
func ListAllUndeliveredAgentMessages() ([]*AgentMessage, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return queryRows(d, scanAgentMessage, `SELECT `+agentMessageColumns+` FROM agent_messages
		WHERE delivered_at IS NULL AND read_at IS NULL AND nudge_cancelled_at IS NULL ORDER BY id ASC`)
}

// ListDeliveredUnreadAgentMessages returns every message that has been
// delivered (delivered_at set — the recipient was nudged at least once) but
// not yet read (read_at empty), across all recipients, oldest first. Rows
// with an empty to_conv (a group-broadcast bookkeeping record, not addressed
// to any single agent) are excluded. This is the candidate set the unread-
// reminder sweep walks: a message the agent was told about but hasn't opened.
//
// Undelivered messages are deliberately NOT included — first delivery is the
// flush-on-online path's job; this sweep only re-nudges about already-
// delivered traffic, so "reminder" means exactly that.
//
// Ordering is by id (insertion order) so reminders have a stable total order
// even when several messages carry the same created_at instant.
func ListDeliveredUnreadAgentMessages() ([]*AgentMessage, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + agentMessageColumns + `
		FROM agent_messages
		WHERE to_conv != '' AND delivered_at IS NOT NULL AND read_at IS NULL
		ORDER BY id ASC`
	return queryRows(db, scanAgentMessage, q)
}

type AgentMessageNudgeClaim struct {
	ClaimedAt int64
	Attempt   int
}

// ClaimAgentMessageNudge atomically acquires a short-lived nudge lease without
// marking the message delivered. The returned token is the exact Unix-nanosecond
// timestamp plus a monotonically-incremented per-message attempt number stored
// by the same UPDATE. Completion/release must present both, so even two claims
// made at the same instant cannot let a stale worker mutate a newer claim.
func ClaimAgentMessageNudge(id int64, now time.Time) (token AgentMessageNudgeClaim, claimed bool, err error) {
	db, err := Open()
	if err != nil {
		return token, false, err
	}
	claimedNS, err := timeToUnixNano(now)
	if err != nil {
		return AgentMessageNudgeClaim{}, false, err
	}
	token.ClaimedAt = claimedNS
	err = db.QueryRow(
		`UPDATE agent_messages
		 SET nudge_claimed_at = ?, nudge_attempted_at = ?, nudge_attempts = nudge_attempts + 1
		 WHERE id = ? AND delivered_at IS NULL AND read_at IS NULL AND nudge_claimed_at IS NULL AND nudge_cancelled_at IS NULL
		 AND NOT EXISTS (SELECT 1 FROM federation_mail_custody c WHERE json_extract(c.payload,'$.local_message')=agent_messages.id AND c.state IN ('queued','handoff'))
		 RETURNING nudge_attempts`,
		claimedNS, claimedNS, id).Scan(&token.Attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentMessageNudgeClaim{}, false, nil
	}
	if err != nil {
		return AgentMessageNudgeClaim{}, false, err
	}
	return token, true, nil
}

// CompleteAgentMessageNudge converts token's in-flight lease into successful
// delivery. The token guard prevents a stale worker from completing a newer
// claim. Returns false when the lease no longer belongs to this worker.
func CompleteAgentMessageNudge(id int64, token AgentMessageNudgeClaim, now time.Time) (bool, error) {
	return CompleteAgentMessageNudgeState(id, token, now, false)
}

// CompleteAgentMessageNudgeState records a successful pane delivery. Inline
// delivery also marks the archival inbox copy read in the same guarded write,
// so unread reminders do not ask the agent to fetch content it already saw.
func CompleteAgentMessageNudgeState(id int64, token AgentMessageNudgeClaim, now time.Time, consumed bool) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var readAt any
	if consumed {
		readAt = dbTime(now)
	}
	res, err := d.Exec(`UPDATE agent_messages
		SET delivered_at = ?, read_at = CASE WHEN ? IS NOT NULL THEN ? ELSE read_at END, nudge_claimed_at = NULL
		WHERE id = ? AND delivered_at IS NULL AND nudge_claimed_at = ? AND nudge_attempts = ?`,
		dbTime(now), readAt, readAt, id, token.ClaimedAt, token.Attempt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

const deleteSupersededCronMessagesSQL = `DELETE FROM agent_messages
	WHERE id IN (
		SELECT older.message_id
		FROM agent_cron_messages older
		JOIN agent_messages older_message ON older_message.id = older.message_id
		WHERE older_message.delivered_at IS NULL AND older_message.read_at IS NULL
		  AND older_message.nudge_claimed_at IS NULL AND older_message.nudge_cancelled_at IS NULL
		  AND EXISTS (
			SELECT 1
			FROM agent_cron_messages newer
			JOIN agent_messages newer_message ON newer_message.id = newer.message_id
			WHERE newer.cron_job_id = older.cron_job_id
			  AND newer.message_id > older.message_id
			  AND (
				(older_message.to_agent != '' AND older_message.pin_gen = 0
				 AND newer_message.to_agent = older_message.to_agent AND newer_message.pin_gen = 0)
				OR
				(older_message.to_agent = '' AND newer_message.to_agent = ''
				 AND newer_message.to_conv = older_message.to_conv)
			  )
		  )
	)`

// ReleaseAgentMessageNudge releases token's lease after a failed send while
// preserving its durable attempt count/time for retry backoff. If a newer tick
// from the same cron job arrived while this row was in flight, the released
// stale row is removed in the same transaction instead of retrying after the
// newer tick.
func ReleaseAgentMessageNudge(id int64, token AgentMessageNudgeClaim) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE agent_messages SET nudge_claimed_at = NULL
		WHERE id = ? AND delivered_at IS NULL AND nudge_claimed_at = ? AND nudge_attempts = ?`,
		id, token.ClaimedAt, token.Attempt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		if _, err := tx.Exec(deleteSupersededCronMessagesSQL); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

// ReleaseAllAgentMessageNudgeClaims clears leases left by the previous daemon
// process. It is safe only at daemon startup, before this process launches any
// delivery worker.
func ReleaseAllAgentMessageNudgeClaims() (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE agent_messages SET nudge_claimed_at = NULL
		WHERE delivered_at IS NULL AND nudge_claimed_at IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(deleteSupersededCronMessagesSQL); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// SuppressOfflineRegularNudgesForAgent discards only the tmux notification
// attempts for an offline agent's head-following regular messages. The durable
// inbox rows remain unread and unprocessed, so they still count toward bounded
// sender backpressure and can be fetched after the agent resumes.
func SuppressOfflineRegularNudgesForAgent(agentID string, now time.Time) (int64, error) {
	if agentID == "" {
		return 0, nil
	}
	return suppressOfflineRegularNudges(
		`to_agent = ? AND pin_gen = 0`, []any{agentID}, now)
}

// SuppressOfflineRegularNudgesForExactConv is the pinned/non-actor counterpart
// to SuppressOfflineRegularNudgesForAgent.
func SuppressOfflineRegularNudgesForExactConv(convID string, now time.Time) (int64, error) {
	if convID == "" {
		return 0, nil
	}
	return suppressOfflineRegularNudges(
		`to_conv = ? AND (pin_gen = 1 OR to_agent = '')`, []any{convID}, now)
}

func suppressOfflineRegularNudges(targetWhere string, targetArgs []any, now time.Time) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	stamp := dbTime(now)
	args := []any{stamp, stamp}
	args = append(args, targetArgs...)
	res, err := d.Exec(`UPDATE agent_messages
		SET delivered_at = ?, nudge_discarded_at = ?, nudge_claimed_at = NULL
		WHERE regular_send = 1 AND delivered_at IS NULL AND nudge_claimed_at IS NULL
		  AND nudge_cancelled_at IS NULL AND `+targetWhere, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CancelAgentMessageNudge durably abandons nudge delivery for one message —
// the reaper's orphan sweep calls it when the recipient is retired or deleted,
// so no pane can ever receive the nudge. The row leaves every undelivered
// predicate but stays readable in the inbox: this is NOT a delivered/read
// stamp. The nudge_claimed_at = ” guard leaves an in-flight delivery alone
// (its worker owns the row until it completes or releases). Returns true iff
// this call cancelled the row, so the caller can log exactly once.
//
// targetAgentID is the actor the caller resolved as unavailable; the UPDATE
// re-validates that verdict atomically with the stamp (`no ACTIVE agents row
// with this id` covers both retired and deleted). Without this, the sweep's
// read-then-write could race ReinstateAgentByID: reinstate clears existing
// cancellations in its transaction, then a sweep holding a stale "retired"
// verdict stamps a fresh one that nothing would ever clear again. SQLite
// serializes the writers, so whichever of reinstate/cancel commits second
// sees the other's effect and converges correctly.
func CancelAgentMessageNudge(id int64, targetAgentID string, now time.Time, reason string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE agent_messages
		SET nudge_cancelled_at = ?, nudge_cancel_reason = ?
		WHERE id = ? AND delivered_at IS NULL AND read_at IS NULL
		  AND nudge_claimed_at IS NULL AND nudge_cancelled_at IS NULL
		  AND NOT EXISTS (
			SELECT 1 FROM agents WHERE agent_id = ? AND (retired_at IS NULL OR EXISTS (SELECT 1 FROM agent_federation_presence p WHERE p.agent_id=agents.agent_id AND p.state='away')))`,
		dbTime(now), reason, id, targetAgentID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// MarkAgentMessageRead records explicit inbox consumption. For bounded regular
// messages this is also the processing acknowledgement that frees capacity;
// notification delivery alone never does.
func MarkAgentMessageRead(id int64) error {
	db, err := Open()
	if err != nil {
		return err
	}
	now := dbTime(time.Now())
	_, err = db.Exec(`UPDATE agent_messages
		SET read_at = CASE WHEN read_at IS NULL THEN ? ELSE read_at END,
			processed_at = CASE WHEN regular_send = 1 AND processed_at IS NULL THEN ? ELSE processed_at END
		WHERE id = ? AND (read_at IS NULL OR (regular_send = 1 AND processed_at IS NULL))`, now, now, id)
	return err
}

// MarkRegularAgentMessageStarted correlates a server-authored injected prompt
// with its durable row. Inline prompts carry the full body, so the hook marks
// them read here as well as started; doing both in one write avoids a race with
// the daemon's post-send delivery completion.
func MarkRegularAgentMessageStarted(id int64, convID string, inline bool, now time.Time) (bool, error) {
	if id <= 0 || convID == "" {
		return false, nil
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	stamp := dbTime(now)
	var readAt any
	if inline {
		readAt = stamp
	}
	targetWhere := `(to_conv = ? OR (pin_gen = 0 AND to_agent != '' AND to_agent =
		COALESCE((SELECT agent_id FROM agent_conversations WHERE conv_id = ?), '')))`
	res, err := tx.Exec(`UPDATE agent_messages
		SET started_at = CASE WHEN started_at IS NULL THEN ? ELSE started_at END,
			read_at = CASE WHEN ? IS NOT NULL THEN ? ELSE read_at END
		WHERE id = ? AND regular_send = 1 AND processed_at IS NULL
		  AND `+targetWhere,
		stamp, readAt, readAt, id, convID, convID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return false, nil
	}
	// Reaching a later injected message proves the harness advanced past any
	// earlier prompt for this recipient. Only already-read rows qualify here,
	// which recovers missed inline hooks without falsely consuming pointer mail.
	//
	// This keeps delivered_at IS NOT NULL while the same watermark in
	// MarkReadRegularAgentMessagesProcessed does not; the difference is
	// deliberate, not drift. Reached from UserPromptSubmit, this statement can
	// run before the daemon's post-send completion stamp lands, so delivered_at
	// distinguishes a genuine pane delivery from a row still mid-flight. By the
	// time a terminal hook runs there is nothing left to disambiguate, and
	// read_at alone already implies the body reached the pane.
	if _, err := tx.Exec(`UPDATE agent_messages SET processed_at = ?
		WHERE id < ? AND regular_send = 1 AND delivered_at IS NOT NULL AND read_at IS NOT NULL AND processed_at IS NULL
		  AND `+targetWhere, stamp, id, convID, convID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// MarkReadRegularAgentMessagesProcessed acknowledges regular messages whose
// full body is already in the recipient's pane, once its turn reaches a
// terminal hook. Pointer notifications remain pending until inbox read marks
// their row processed.
//
// read_at is deliberately the only delivery predicate here, which rests on an
// invariant worth stating explicitly: on a regular_send row, read_at != ” with
// processed_at == ” can only mean inline pane delivery. Exactly three writers
// produce that state, and all three put the full body in the pane:
//
//   - MarkAgentMessageDeliveredState (the direct-send twin), consumed only
//   - CompleteAgentMessageNudgeState (the daemon's post-send stamp), consumed only
//   - the inline branch of MarkRegularAgentMessageStarted
//
// Every explicit-read path — MarkAgentMessageRead, SetAgentMessagesRead,
// MarkAgentMailboxRead — stamps read_at and processed_at together on a regular
// row, so it never lands here. Pointer nudges leave read_at empty entirely.
// TestExplicitReadPathsNeverLeaveRegularMailReadButUnprocessed pins that
// coupling: a future path that marks a regular row read without processing it
// would make this sweep silently acknowledge mail the agent never fetched.
//
// Requiring started_at as well used to wedge the recipient permanently
// (TCL-737): an inline turn that missed its UserPromptSubmit correlation, or
// whose Stop hook never fired, left a row read-but-unprocessed. Nothing showed
// it as unread, the backlog gate counted it, and the only self-heal — the
// catch-up in MarkRegularAgentMessageStarted — needed a NEW inline message that
// the full queue rejected first.
//
// The id watermark is what keeps this from trading that deadlock for an
// unbounded queue. Delivery is not gated on the pane being idle
// (deliverablePane admits a working pane), so mail sent to a busy recipient is
// send-keys'd mid-turn and waits in the harness input box. Acknowledging every
// read row on each Stop would therefore bound admissions per turn rather than
// outstanding work: a sender faster than the recipient's turn rate would be
// readmitted a full queue every turn while the recipient consumed one prompt,
// growing the queued-prompt backlog without limit. So instead:
//
//   - When a prompt did begin this turn, acknowledge every read row up to the
//     NEWEST one that began. Reaching a later prompt proves the harness
//     advanced past the earlier ones, which sweeps up rows whose own hooks were
//     missed while still admitting only what the recipient actually consumed —
//     one row per turn under steady traffic, exactly as before.
//   - When none began, acknowledge the single OLDEST read row. That is the
//     deadlock escape: a fully wedged queue reopens by one slot per completed
//     turn, so it always drains without operator intervention, and a recipient
//     busy with non-mail work still cannot be flooded.
//
// The result only ever acknowledges MORE than the old started_at predicate did,
// never less: every started-and-read row is by construction at or below the
// watermark.
//
// Which arm carries the load depends on the recipient. For an agent whose
// prompt correlation works, the MAX arm is the normal path and the fallback
// almost never fires. For an agent with broken correlation — the population
// this ticket is about, where all ten live rows had started_at empty — it is
// the other way round: MAX stays NULL and the fallback carries everything, one
// row per completed turn. That regime is self-sustaining, because once the
// fallback acknowledges a row, that row's own later UserPromptSubmit hits
// MarkRegularAgentMessageStarted's processed_at = ” guard and never stamps
// started_at. Both regimes drain; neither can wedge.
//
// Both subqueries MUST stay non-correlated — they may reference bound
// parameters only, never the outer row. SQLite evaluates a non-correlated
// scalar subquery once into a register; scoping an inner WHERE by the outer row
// instead (say to_conv = agent_messages.to_conv, which compiles and reads
// perfectly sensibly) would make it correlated and re-evaluated per row, so
// each acknowledged row would advance MIN(id) to the next and the fallback
// would cascade from "release one" into "release the whole queue" — silently
// restoring the unbounded-admissions defect the watermark exists to prevent.
// TestMessageBackpressureInlineMailDrainsOnNextTerminalHook is the cascade
// test: it drains exactly 1 of 10 and needs 9 further turns to finish.
//
// The empty-to_agent arm mirrors countUnprocessedRegularMessageBacklog.
// Messages sent before a conv is enrolled have no stable-recipient companion;
// after a generation rotation they still belong to the actor through the old
// conv's agent_conversations row. Without that arm they consume the live
// actor's backlog capacity but no terminal hook can ever acknowledge them
// (TCL-739).
func MarkReadRegularAgentMessagesProcessed(convID string, now time.Time) (int64, error) {
	if convID == "" {
		return 0, nil
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	const recipient = `(to_conv = ? OR
		(pin_gen = 0 AND to_agent != '' AND to_agent =
			COALESCE((SELECT agent_id FROM agent_conversations WHERE conv_id = ?), '')) OR
		(to_agent = '' AND to_conv IN (
			SELECT conv_id FROM agent_conversations WHERE agent_id =
				COALESCE((SELECT agent_id FROM agent_conversations WHERE conv_id = ?), ''))))`
	stamp := dbTime(now)
	res, err := d.Exec(`UPDATE agent_messages SET processed_at = ?
		WHERE regular_send = 1 AND read_at IS NOT NULL AND processed_at IS NULL
		  AND `+recipient+`
		  AND id <= COALESCE(
			(SELECT MAX(id) FROM agent_messages
			  WHERE regular_send = 1 AND read_at IS NOT NULL AND processed_at IS NULL AND started_at IS NOT NULL
				AND `+recipient+`),
			(SELECT MIN(id) FROM agent_messages
			  WHERE regular_send = 1 AND read_at IS NOT NULL AND processed_at IS NULL
				AND `+recipient+`))`,
		stamp,
		convID, convID, convID,
		convID, convID, convID,
		convID, convID, convID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteAgentMessageByID deletes a single agent_messages row when
// forConv is the sender or recipient. Returns (deleted, err) where
// deleted is true iff a row was removed (false also when the row
// exists but forConv is neither party — the caller is expected to
// distinguish those cases via GetAgentMessage). Mirrors the prune
// auth model: if you're a party to the message you can wipe the
// shared row.
func DeleteAgentMessageByID(id int64, forConv string) (bool, error) {
	if forConv == "" {
		return false, fmt.Errorf("forConv required")
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(
		`DELETE FROM agent_messages WHERE id = ? AND (from_conv = ? OR to_conv = ?)`,
		id, forConv, forConv)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteAgentMessageForActor is the actor-keyed twin of
// DeleteAgentMessageByID: it authorises the wipe when the caller is a party to
// the message by current conv (from_conv/to_conv) OR stable actor
// (from_agent/to_agent). The agent terms let a row received or sent under a
// predecessor generation be deleted after the agent reincarnated / ran /clear
// (JOH-317); the conv terms are kept alongside (NOT replaced) so a row whose
// agent companion is ” — a conv messaged before it enrolled — stays
// deletable from its current conv. Whichever of conv/agentID is empty is
// skipped (see actorMatchClause) rather than emitted as `col = ”`, which would
// match non-actor rows; a non-actor caller is authorised by conv alone.
func DeleteAgentMessageForActor(id int64, conv, agentID string) (bool, error) {
	where, args := actorMatchClause(
		[2]string{"from_conv", conv}, [2]string{"to_conv", conv},
		[2]string{"from_agent", agentID}, [2]string{"to_agent", agentID})
	if where == "" {
		return false, fmt.Errorf("conv or agentID required")
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(
		`DELETE FROM agent_messages WHERE id = ? AND `+where,
		append([]any{id}, args...)...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GetAgentMessage returns a single message by ID, or nil if not found.
func GetAgentMessage(id int64) (*AgentMessage, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(`SELECT `+agentMessageColumns+`
		FROM agent_messages WHERE id = ?`, id)
	m, err := scanAgentMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// ListAgentMessagesForConv returns the most recent messages for a recipient.
// limit <= 0 means "no limit".
func ListAgentMessagesForConv(toConv string, limit int) ([]*AgentMessage, error) {
	return listAgentMessagesByCol("to_conv", toConv, limit)
}

// ListAgentMessagesFromConv returns the most recent messages a given
// conv-id sent (the "outbox" view). Symmetric with ListAgentMessagesForConv.
func ListAgentMessagesFromConv(fromConv string, limit int) ([]*AgentMessage, error) {
	return listAgentMessagesByCol("from_conv", fromConv, limit)
}

// ListInboxForActor is the actor-keyed inbox: every message addressed to the
// caller either by its current conv (to_conv) OR by its stable actor
// (to_agent), most recent first. The to_agent term — stamped from to_conv at
// insert by the v79 dual-write (JOH-281/284) and shared by every generation
// of the agent — is what spans ALL the actor's conv generations as ONE inbox,
// the rotation fix (JOH-317): mail received before the agent reincarnated /
// ran /clear is still readable from the live generation instead of being
// stranded on the predecessor conv. The to_conv term is kept alongside it
// (NOT replaced) so a message whose to_agent companion is ” — a conv that
// was messaged before it enrolled as an agent, which is never backfilled —
// stays visible in its own current inbox. This mirrors the delivery layer,
// which likewise keeps both an agent-keyed and a conv-keyed drain so the two
// together cover the whole set. pin_gen rows are NOT excluded — pinning only
// steers DELIVERY (which pane is nudged); for the inbox VIEW the message is
// still the actor's mail.
//
// Known limitation: a row addressed to a PAST generation that ALSO has
// to_agent=” (a conv messaged before it enrolled, that later rotated) is
// reachable from neither term once the actor moves on, and stays stranded. It
// needs both "messaged-before-enroll" AND "later rotated", which is rare; a
// full fix would OR in every one of the actor's conv generations. All four
// read/delete/prune paths agree on missing it, so there is no asymmetry.
func ListInboxForActor(toConv, toAgent string, limit int) ([]*AgentMessage, error) {
	return listAgentMessagesForActor("to_conv", "to_agent", toConv, toAgent, "", limit)
}

// ListUnreadInboxForActor is ListInboxForActor narrowed to unread mail before the
// limit applies, so old unread mail isn't pushed out of view by newer read mail.
func ListUnreadInboxForActor(toConv, toAgent string, limit int) ([]*AgentMessage, error) {
	return listAgentMessagesForActor("to_conv", "to_agent", toConv, toAgent, " AND read_at IS NULL", limit)
}

// ListOutboxForActor is the actor-keyed outbox twin of ListInboxForActor:
// every message the caller sent by current conv (from_conv) OR stable actor
// (from_agent), most recent first.
func ListOutboxForActor(fromConv, fromAgent string, limit int) ([]*AgentMessage, error) {
	return listAgentMessagesForActor("from_conv", "from_agent", fromConv, fromAgent, "", limit)
}

// actorMatchClause builds the "(col = ? OR …)" predicate that matches a
// message to its caller by current conv and/or stable actor, used by the
// actor-keyed inbox/outbox/delete/prune. Each (column, value) pair is included
// ONLY when its value is non-empty: an empty value is skipped entirely rather
// than emitted as `col = ”`, which would over-match every non-actor /
// group-broadcast row (empty to_conv/to_agent). Returns ("", nil) when every
// value is empty — the caller must treat that as "match nothing". Column names
// are literal constants from the call sites, never user input.
func actorMatchClause(pairs ...[2]string) (string, []any) {
	var terms []string
	var args []any
	for _, p := range pairs {
		if p[1] == "" {
			continue
		}
		terms = append(terms, p[0]+" = ?")
		args = append(args, p[1])
	}
	if len(terms) == 0 {
		return "", nil
	}
	return "(" + strings.Join(terms, " OR ") + ")", args
}

// listAgentMessagesForActor shares the read/scan path for the actor-keyed
// inbox/outbox: it selects rows matching convCol = conv OR agentCol = agent,
// ordered most-recent-first, skipping whichever of conv/agent is empty (see
// actorMatchClause). convCol/agentCol must be literal column names, never user
// input — they are interpolated into SQL. Ordering by id DESC gives the actor
// mailbox a stable newest-insertion-first total order.
func listAgentMessagesForActor(convCol, agentCol, conv, agent, filter string, limit int) ([]*AgentMessage, error) {
	where, args := actorMatchClause([2]string{convCol, conv}, [2]string{agentCol, agent})
	if where == "" {
		return nil, nil
	}
	db, err := Open()
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + agentMessageColumns + `
		FROM agent_messages WHERE ` + where + filter + ` ORDER BY id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	return queryRows(db, scanAgentMessage, q, args...)
}

// listAgentMessagesByCol shares the read/scan path between the inbox
// and outbox queries. col must be a literal column name (`to_conv`,
// `from_conv`, `to_agent` or `from_agent`), never user input — it's
// interpolated into SQL.
//
// Ordering is by id DESC because the mailbox contract is newest insertion
// first. The autoincrement key supplies a unique total order for LIMIT even
// when multiple rows have the same created_at instant.
func listAgentMessagesByCol(col, value string, limit int) ([]*AgentMessage, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + agentMessageColumns + `
		FROM agent_messages WHERE ` + col + ` = ? ORDER BY id DESC`
	args := []any{value}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := db.Query(q, args...)
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
