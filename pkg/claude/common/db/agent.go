package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// AgentGroup is a row in agent_groups.
type AgentGroup struct {
	ID                int64
	Name              string
	Descr             string
	DefaultCwd        string                           // pre-filled cwd for agents spawned into this group; "" = none
	DefaultSpawnGroup bool                             // operator-selected tie-breaker when directory auto-join matches multiple groups
	DefaultContext    string                           // shared startup context delivered to the inbox of agents spawned into this group; "" = none
	DefaultProfile    string                           // current name of the stable spawn-profile reference whose launch fields fill blanks server-side; "" = none
	DefaultPrompts    []string                         // ordered saved-prompt names composed into a spawned agent's initial message when the spawn names none
	Environment       []sandboxpolicy.EnvironmentEntry // common process environment for fresh spawns
	SandboxProfile    string                           // current name of the stable sandbox-profile assignment; "" = none
	SandboxProfileID  int64                            `json:"-"`
	MaxMembers        int                              // hard cap on member count; a spawn that would exceed it is refused. 0 = unlimited
	NotifyEnabled     bool                             // OS notifications for member agents; false mutes the whole group (a per-agent 'on' pref still overrides)
	// ReinjectAfterCompact is what tclaude re-injects into member agents after
	// a compaction or /clear boundary: one of the ReinjectAfterCompact*
	// values, "" meaning the default (ReinjectContexts).
	ReinjectAfterCompact string
	RemoteControl        *bool // remote-control policy for agents spawned into this group; tri-state: nil = inherit (defer to the spawn profile), false = actively deny (force off), true = actively opt-in (force on). Overrides the profile default (JOH-262)
	// AttachmentURL is the group's optional persistent http(s) reference
	// (a Linear project, GitHub board, design doc, …). AttachmentLabel is an
	// optional display override; callers derive a compact label when blank.
	AttachmentURL   string
	AttachmentLabel string
	// Mission and SourceTemplate are deployment provenance (JOH-245): what a
	// task force was deployed against (a free-text topic or Linear link) and the
	// template it was instantiated/deployed from (surfaced as its current name
	// through a stable template id). "" for a group not created from a template
	// (the dashboard reads both-blank as "not a deployed force").
	Mission        string
	SourceTemplate string
	// MissionHeader is the heading the mission is delivered under ("Mission"
	// for a deploy, "Task" for a quick task force). Non-empty means the mission
	// is kept apart from DefaultContext and delivered to every member as its
	// own block, whatever include_group_context says (MissionBlock). Empty
	// means legacy: a group deployed before the split, whose mission is folded
	// into DefaultContext, or a group with no mission.
	MissionHeader string
	// SourceTemplateID is the durable provenance link. The name remains a
	// historical display fallback if the source template is later deleted.
	SourceTemplateID int64 `json:"-"`
	CreatedAt        time.Time
	ArchivedAt       time.Time // zero = active; non-zero = archived (soft-deleted)
	// ParentGroupID nests this group under another (n-level groups-in-groups,
	// JOH-392). nil = top-level. Referenced by ID (not name) so it survives a
	// parent rename, and the column is declared ON DELETE SET NULL so deleting
	// the parent auto-orphans children back to top-level — see migrateV98toV99.
	// v1 is structure-only: it shapes the dashboard tree but does not (yet)
	// inherit permissions, message routing, or spawn-target down the tree.
	ParentGroupID *int64
	// RouteGeneration advances on every membership mutation. Route records
	// snapshot it so an offline roster change invalidates stale authority.
	RouteGeneration int64
	// OwnerScopesJSON narrows the STRUCTURAL owner-implied permission bypass
	// for THIS group (TCL-1071): canonical JSON mapping a permission slug to
	// the scope an owner's bypass is confined to, e.g.
	// {"groups.members.spawn":{"spawn_profile":["p1"]}}. "" (the default) means the
	// historical unrestricted bypass. It never touches an EXPLICIT grant the
	// owner holds — those resolve first, under the ordinary precedence — only
	// the gap-filling bypass that fires when nothing else decided.
	OwnerScopesJSON string
}

// IsArchived reports whether the group has been soft-deleted via
// `groups archive`. Archived groups are hidden from default listings
// and reject mutating operations (groups.members.add / groups.members.remove /
// owners.* / message), while preserving message history.
func (g *AgentGroup) IsArchived() bool {
	return !g.ArchivedAt.IsZero()
}

// AgentGroupMember is a row in agent_group_members.
//
// A member has no per-group name: an agent's single name is its
// conversation title (conv_index.custom_title). Role and Descr carry
// the per-group semantics.
type AgentGroupMember struct {
	GroupID  int64
	ConvID   string
	Role     string
	Descr    string
	JoinedAt time.Time
}

// ReincarnationHandoffSubject is the subject the reincarnate path stamps
// on the handoff message it queues from a retiring predecessor to its live
// successor. It is a single source of truth shared by the writer
// (agentd.reincarnate) and the firehose filter (MailboxFilter.where), which
// carves these rows out of the retired-agent exclusion so the handoff still
// shows in "All agent messages" even though its sender is now retired.
const ReincarnationHandoffSubject = "reincarnation handoff"

// StartupContextSubject identifies the one mailbox row whose delivery is
// owned by the harness launch path rather than the regular post-startup nudge
// pipeline. Keep this marker central: regular-message inlining must never race
// the CLI-argument seed/welcome and duplicate a spawned agent's briefing.
const StartupContextSubject = "Startup context"

// AgentMessage is a row in agent_messages. Body is stored inline.
// ParentID is the message this one is a reply to, or 0 for top-of-thread.
//
// ToRecipients / CcRecipients are the email-style audience of the
// original send: every row of a multi-recipient send carries the same
// arrays (denormalized) so each recipient knows who else got the
// message. Empty for legacy single-recipient sends — ToConv stays
// canonical for delivery + filtering, the recipient arrays are
// display-only.
//
// ToRecipientAgents / CcRecipientAgents are the stable agent_id companions
// of those audience conv arrays (JOH-284), persisted at insert and indexed
// 1:1 with the conv arrays (entry i is the actor of conv entry i, "" for a
// non-actor / unmapped conv). They let the To:/CC: headers render the
// rotation-immune id straight from the row — surviving recipient-generation
// pruning (DeleteAgentByConvID), which strips the conv→agent link the old
// read-time resolution relied on. Empty for legacy pre-v79 rows the backfill
// missed; readers fall back to a live conv→agent lookup then.
type AgentMessage struct {
	ID       int64
	GroupID  int64
	FromConv string
	ToConv   string
	// OriginalToConv is non-empty when the send path rewrote a
	// superseded conv-id onto a live successor (db.ResolveLatestConv
	// followed the agent_conv_succession chain forward). Records the
	// id the sender originally addressed so the recipient's `inbox
	// read` can render an `Original-To: <id>` header. Empty for the
	// usual case where ToConv was already canonical.
	OriginalToConv string
	// FromAgent / ToAgent are the stable agent_id of the sender and
	// recipient (JOH-27 PR3a), denormalised alongside From/ToConv so a
	// message renders `name (agent_id)` without a conv→agent lookup and
	// stays attributable after the conv is pruned. They are DERIVED on
	// write: InsertAgentMessage always recomputes them from From/ToConv
	// (any value preset on the struct is ignored), and they are '' when
	// the conv is not an actor (a plain conv, or a since-deleted agent).
	// On read, scanAgentMessage fills them from the stored columns.
	FromAgent   string
	ToAgent     string
	Subject     string
	Body        string
	ParentID    int64
	CreatedAt   time.Time
	DeliveredAt time.Time
	ReadAt      time.Time
	// NudgeClaimedAt is a short-lived delivery lease. It is separate from
	// DeliveredAt so a failed/hung send never masquerades as success.
	// NudgeAttemptedAt/NudgeAttempts persist retry history across daemon
	// restarts and drive bounded per-message backoff.
	NudgeClaimedAt   time.Time
	NudgeAttemptedAt time.Time
	NudgeAttempts    int
	// NudgeCancelledAt marks nudge delivery as permanently abandoned — the
	// reaper's orphan sweep stamps it when the recipient is retired or deleted,
	// so no drain can ever reach a pane. A cancelled row leaves every
	// undelivered predicate (drains, queue depths, the stale-queue watchdog)
	// but the message itself stays readable in the inbox; it is NOT a
	// delivered/read claim. Reinstating a retired agent clears it so queued
	// mail resumes delivery. NudgeCancelReason is the human-readable why.
	NudgeCancelledAt  time.Time
	NudgeCancelReason string
	// RegularSend identifies bounded human/agent one-shot messages. Their
	// processing lifecycle is independent of nudge delivery: StartedAt is
	// stamped when the exact injected prompt begins, ProcessedAt when that
	// inline turn ends or the recipient explicitly reads the inbox row, and
	// NudgeDiscardedAt records an offline notification attempt deliberately
	// suppressed without discarding the durable unread message.
	//
	// StartedAt no longer gates whether a row is acknowledged — a missed
	// UserPromptSubmit correlation must not pin a queue slot (TCL-737). It
	// survives as the watermark input to MarkReadRegularAgentMessagesProcessed
	// ("the newest prompt this recipient actually began") and as diagnostics;
	// do not reintroduce it as a required predicate.
	RegularSend       bool
	StartedAt         time.Time
	ProcessedAt       time.Time
	NudgeDiscardedAt  time.Time
	ToRecipients      []string
	CcRecipients      []string
	ToRecipientAgents []string
	CcRecipientAgents []string
	// PinGen marks a message deliberately addressed to a SPECIFIC previous
	// generation of the recipient agent (JOH-310 prev-gen targeting): set
	// when the sender passed an explicit `gen` conv-id. Normal messages
	// (PinGen=false) follow the agent to its current head generation at
	// delivery time; a pinned message sticks to its recorded ToConv. Unlike
	// FromAgent/ToAgent it is NOT derived — it is explicit caller intent,
	// persisted by InsertAgentMessage and read back by scanAgentMessage.
	PinGen bool
	// OperatorAuthored is persisted in operator_agent_messages rather than the
	// legacy sender columns. Empty FromConv is also used by internal system
	// handoffs, so this explicit marker is the only safe human attribution.
	OperatorAuthored bool
}

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

func formatTimeOrEmpty(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return dbTime(t)
}

// recipientsToJSON encodes a recipient slice as the JSON-array text we
// store in agent_messages.{to_recipients,cc_recipients}. Empty / nil
// slices encode to "" so the column matches the v18 default of empty
// string for legacy rows (round-trip-friendly).
func recipientsToJSON(rs []string) string {
	if len(rs) == 0 {
		return ""
	}
	b, err := json.Marshal(rs)
	if err != nil {
		return ""
	}
	return string(b)
}

// recipientsFromJSON decodes the column back into a slice. Empty
// string decodes to nil; malformed input also yields nil rather than
// failing the whole row read — the recipients arrays are display-only,
// not load-bearing for delivery.
func recipientsFromJSON(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// recipientAgentsJSON resolves an audience conv-id array to the parallel JSON
// array of stable agent_ids we store in the {to,cc}_recipient_agents companion
// columns (JOH-284). The result indexes 1:1 with convs — entry i is the actor
// owning convs[i], or "" for a non-actor / unmapped / since-pruned conv — so a
// reader can pair them by index and fall back to a live lookup only on the
// empty gaps. Returns "" (the column's empty default) when convs is empty OR
// none of them resolve to an actor, so a send with no resolvable audience
// stores the same empty value a legacy row carries and the reader falls back
// wholesale. q is any
// open *sql.DB / *sql.Tx (the resolution is the same agent_conversations join
// InsertAgentMessage and migrateV75toV76 use, so stored and backfilled rows
// agree).
func recipientAgentsJSON(q dbExecQuerier, convs []string) string {
	if len(convs) == 0 {
		return ""
	}
	agents := make([]string, len(convs))
	any := false
	for i, c := range convs {
		if c == "" {
			continue
		}
		if id, err := agentIDForConvTx(q, c); err == nil && id != "" {
			agents[i] = id
			any = true
		}
	}
	if !any {
		return ""
	}
	return recipientsToJSON(agents)
}

func parseTimeOrZero(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := parseLegacyDBTime(s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// rowScanner abstracts *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanAgentGroup(s rowScanner) (*AgentGroup, error) {
	var g AgentGroup
	var createdAt, archivedAt dbTimestamp
	var remoteControl, parentID sql.NullInt64
	var environmentJSON, defaultPrompts string
	if err := s.Scan(&g.ID, &g.Name, &g.Descr, &g.DefaultCwd, &g.DefaultSpawnGroup, &g.DefaultContext,
		&g.DefaultProfile, &g.SandboxProfile, &g.SandboxProfileID,
		&g.MaxMembers, &g.NotifyEnabled, &remoteControl, &g.AttachmentURL, &g.AttachmentLabel, &g.Mission,
		&g.MissionHeader, &g.SourceTemplate, &g.SourceTemplateID, &createdAt, &archivedAt, &parentID, &g.RouteGeneration,
		&g.OwnerScopesJSON, &environmentJSON, &g.ReinjectAfterCompact, &defaultPrompts); err != nil {
		return nil, err
	}
	environment, err := unmarshalEnvironmentColumn(environmentJSON)
	if err != nil {
		return nil, fmt.Errorf("decode group %q environment: %w", g.Name, err)
	}
	g.Environment = environment
	g.DefaultPrompts = UnmarshalPromptRefs(defaultPrompts)
	g.RemoteControl = nullToBoolPtr(remoteControl)
	g.ParentGroupID = nullToInt64Ptr(parentID)
	g.CreatedAt = createdAt.Time()
	g.ArchivedAt = archivedAt.Time()
	return &g, nil
}

func scanAgentGroupMember(s rowScanner) (*AgentGroupMember, error) {
	var m AgentGroupMember
	var joinedAt dbTimestamp
	if err := s.Scan(&m.GroupID, &m.ConvID, &m.Role, &m.Descr, &joinedAt); err != nil {
		return nil, err
	}
	m.JoinedAt = joinedAt.Time()
	return &m, nil
}

// agentMessageColumns is the canonical, ordered SELECT list for the
// agent_messages columns scanAgentMessage reads — the two MUST stay in
// lockstep (same columns, same order). Every query that feeds
// scanAgentMessage selects exactly this list, so adding a column is a
// single edit here + the matching Scan destination, not a five-call-site
// hunt.
//
// A drifted copy of this list — one SELECT missing the columns a later
// migration added — is the arity-mismatch bug that RED'd main when v79's
// audience-agent columns landed: scanAgentMessage grew to 17 destinations
// but ListDeliveredUnreadAgentMessages still selected 15 ("expected 15
// destination arguments in Scan, not 17"). Centralising the list kills
// that whole bug class. Append-only: add new columns at the END (and a
// matching Scan dest) so existing positional scans stay valid.
const agentMessageColumns = `id, group_id, from_conv, to_conv, from_agent, to_agent, ` +
	`subject, body, parent_id, created_at, delivered_at, read_at, ` +
	`to_recipients, cc_recipients, to_recipient_agents, cc_recipient_agents, ` +
	`original_to_conv, pin_gen, nudge_claimed_at, nudge_attempted_at, nudge_attempts, ` +
	`nudge_cancelled_at, nudge_cancel_reason, regular_send, started_at, processed_at, nudge_discarded_at`

func scanAgentMessage(s rowScanner) (*AgentMessage, error) {
	var m AgentMessage
	var createdAt dbTimestamp
	var deliveredAt, readAt, nudgeClaimedAt, nudgeAttemptedAt dbTimestamp
	var nudgeCancelledAt, startedAt, processedAt, nudgeDiscardedAt dbTimestamp
	var toRecipients, ccRecipients string
	var toRecipientAgents, ccRecipientAgents string
	var pinGen, regularSend int
	if err := s.Scan(&m.ID, &m.GroupID, &m.FromConv, &m.ToConv,
		&m.FromAgent, &m.ToAgent,
		&m.Subject, &m.Body, &m.ParentID,
		&createdAt, &deliveredAt, &readAt,
		&toRecipients, &ccRecipients,
		&toRecipientAgents, &ccRecipientAgents, &m.OriginalToConv, &pinGen,
		&nudgeClaimedAt, &nudgeAttemptedAt, &m.NudgeAttempts,
		&nudgeCancelledAt, &m.NudgeCancelReason, &regularSend,
		&startedAt, &processedAt, &nudgeDiscardedAt); err != nil {
		return nil, err
	}
	m.CreatedAt = createdAt.Time()
	m.DeliveredAt = deliveredAt.Time()
	m.ReadAt = readAt.Time()
	m.NudgeClaimedAt = nudgeClaimedAt.Time()
	m.NudgeAttemptedAt = nudgeAttemptedAt.Time()
	m.NudgeCancelledAt = nudgeCancelledAt.Time()
	m.StartedAt = startedAt.Time()
	m.ProcessedAt = processedAt.Time()
	m.NudgeDiscardedAt = nudgeDiscardedAt.Time()
	m.ToRecipients = recipientsFromJSON(toRecipients)
	m.CcRecipients = recipientsFromJSON(ccRecipients)
	m.ToRecipientAgents = recipientsFromJSON(toRecipientAgents)
	m.CcRecipientAgents = recipientsFromJSON(ccRecipientAgents)
	m.PinGen = pinGen != 0
	m.RegularSend = regularSend != 0
	return &m, nil
}
