package db

import (
	"database/sql"
	"errors"
	"time"
	"unicode/utf8"
)

// Scribe chat replies (agent_turn_replies, schema v263).
//
// The process editor's docked scribe chat shows the scribe's final message of
// each turn. Nothing else persists assistant turns, so the Stop hook records
// them here — for process scribes ONLY. The scribe check is made here,
// server-side against group membership, never from anything the hook payload
// claims: a conv is a scribe when its agent is a member with role "scribe"
// of a group stamped with ScribeGroupDescr.

// ScribeGroupDescr marks the shared group the scribe-summon machinery creates
// (agentd/scribe.go); it is the provenance check for scribe groups.
const ScribeGroupDescr = "Ad-hoc scribe agent — summoned from the dashboard to edit tclaude state by chat (JOH-361)."

const (
	// ScribeTurnReplyKeep bounds the ring per agent.
	ScribeTurnReplyKeep = 40
	// ScribeTurnReplyMaxBytes caps one stored reply.
	ScribeTurnReplyMaxBytes = 16 << 10
)

// ScribeTurnReply is one stored scribe turn.
type ScribeTurnReply struct {
	ID        int64
	ConvID    string
	Body      string
	CreatedAt time.Time
}

func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ellipsis = "…"
	cut := max - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// RecordScribeTurnReply stores body as convID's latest turn reply when convID
// belongs to a process scribe, and prunes that agent's ring. It reports
// whether a row was stored; a non-scribe conv or an empty body is a no-op.
func RecordScribeTurnReply(convID, body string, now time.Time) (bool, error) {
	if convID == "" || body == "" {
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
	var agentID string
	err = tx.QueryRow(`
		SELECT ac.agent_id FROM agent_conversations ac
		WHERE ac.conv_id = ? AND EXISTS (
			SELECT 1 FROM agent_group_members m
			JOIN agent_groups g ON g.id = m.group_id
			WHERE m.agent_id = ac.agent_id AND m.role = 'scribe' AND g.descr = ?)`,
		convID, ScribeGroupDescr).Scan(&agentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO agent_turn_replies (agent_id, conv_id, body, created_at) VALUES (?, ?, ?, ?)`,
		agentID, convID, truncateUTF8(body, ScribeTurnReplyMaxBytes), dbTime(now)); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM agent_turn_replies WHERE agent_id = ? AND id NOT IN (
		SELECT id FROM agent_turn_replies WHERE agent_id = ? ORDER BY id DESC LIMIT ?)`,
		agentID, agentID, ScribeTurnReplyKeep); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ListScribeTurnReplies returns agentID's stored replies with id > afterID,
// oldest first.
func ListScribeTurnReplies(agentID string, afterID int64) ([]ScribeTurnReply, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id, conv_id, body, created_at FROM agent_turn_replies
		WHERE agent_id = ? AND id > ? ORDER BY id`, agentID, afterID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ScribeTurnReply
	for rows.Next() {
		var r ScribeTurnReply
		var ns int64
		if err := rows.Scan(&r.ID, &r.ConvID, &r.Body, &ns); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(0, ns)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ScribeChatPrompt is one human→scribe operator message.
type ScribeChatPrompt struct {
	ID        int64
	Subject   string
	Body      string
	CreatedAt time.Time
}

// ListOperatorMessagesToAgent returns the most recent `limit` human-authored
// (operator_agent_messages) messages addressed to agentID, oldest first.
func ListOperatorMessagesToAgent(agentID string, limit int) ([]ScribeChatPrompt, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id, subject, body, created_at FROM (
		SELECT m.id, m.subject, m.body, m.created_at FROM agent_messages m
		JOIN operator_agent_messages o ON o.message_id = m.id
		WHERE m.to_agent = ? ORDER BY m.id DESC LIMIT ?) ORDER BY id`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ScribeChatPrompt
	for rows.Next() {
		var p ScribeChatPrompt
		var ns int64
		if err := rows.Scan(&p.ID, &p.Subject, &p.Body, &ns); err != nil {
			return nil, err
		}
		p.CreatedAt = time.Unix(0, ns)
		out = append(out, p)
	}
	return out, rows.Err()
}

// IsScribeAgent reports whether agentID is a process scribe by the same
// membership rule RecordScribeTurnReply applies.
func IsScribeAgent(agentID string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var n int
	err = d.QueryRow(`SELECT COUNT(*) FROM agent_group_members m
		JOIN agent_groups g ON g.id = m.group_id
		WHERE m.agent_id = ? AND m.role = 'scribe' AND g.descr = ?`, agentID, ScribeGroupDescr).Scan(&n)
	return n > 0, err
}
