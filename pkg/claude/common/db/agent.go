package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

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
