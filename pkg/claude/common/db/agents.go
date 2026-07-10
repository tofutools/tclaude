package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Stable agent identity (JOH-26).
//
// `agents` + `agent_conversations` decouple an agent's durable actor
// identity from the harness conversation generation it currently runs as:
//
//   - agent_id        — the stable actor key. The subject for groups,
//     permissions, ownership, sudo, notification prefs and agent-level
//     lifecycle. Never rotates: reincarnate and Claude Code's /clear mint a
//     fresh conv_id but keep the agent_id.
//   - conv_id         — one harness conversation generation. Still the
//     locator for conversation storage, messages, sessions, costs, history,
//     search and live delivery.
//   - agents.current_conv_id — the conversation generation the actor is
//     live as right now; advanced (not rekeyed) on every rotation.
//
// agent_conversations maps every known conversation generation to its owning
// actor, so a stale reference to any past generation still resolves to the
// live agent. `agent_conv_succession` is retained as conv-generation history
// / stale-selector redirect (db.ResolveLatestConv) — it is no longer the
// identity source of truth.
//
// PR1 (this file) is additive: it stands up the tables, the resolver/CRUD
// API and an idempotent backfill, and the lifecycle paths dual-write into it.
// Authorization still reads the conv-keyed identity tables unchanged; the
// cutover to agent_id-keyed authz lands in a later stage.

// AgentIDPrefix tags a stable agent id so it is unmistakable next to a
// conv-id (a bare UUID) in logs and the DB. 16 random bytes ≈ 128 bits of
// entropy — collision-free for this single-operator tool. Exported so the
// selector resolver can recognise an `agt_`-tagged selector and route it
// straight to the actor layer.
const AgentIDPrefix = "agt_"

// Agent is a row in `agents` — the durable actor identity. Retire fields,
// created_via and pending_name carry the actor-level facts that used to live
// (conv-keyed) on agent_enrollment.
type Agent struct {
	AgentID       string
	CurrentConvID string
	CreatedAt     time.Time
	CreatedVia    string    // spawn | clone | reincarnate | clear | cli | group | grant | promote | backfill | …
	RetiredAt     time.Time // zero ⇒ active
	RetiredBy     string
	// RetiredByAgent is the stable agent_id of the actor that performed the
	// retire (JOH-306), the durable companion to RetiredBy. RetiredBy keeps
	// the raw audit/snapshot value — a conv-id when an agent retired this one,
	// or a literal ("human", "system:export-clone") — while this column carries
	// the rotation-immune actor key derived from it. Empty when the retirer was
	// not an agent (a human/system literal) or its conv could not be resolved.
	RetiredByAgent string
	RetireReason   string
	// PendingName is the actor's intended display name (the spawn-time
	// `--name`), the rescan-immune fallback agent.FreshTitle consults before
	// a real /rename has landed. Actor-level: it survives every rotation.
	PendingName string
}

// Active reports whether the agent is live (not retired). A nil receiver is
// not active.
func (a *Agent) Active() bool { return a != nil && a.RetiredAt.IsZero() }

// AgentConversation is a row in `agent_conversations` — one conversation
// generation mapped to its owning actor.
type AgentConversation struct {
	ConvID   string
	AgentID  string
	Role     string // head | generation | "" — advisory, for forensics
	Reason   string // why this generation was linked: spawn | reincarnate | clear | clone | backfill | …
	LinkedAt time.Time
}

// Conversation roles recorded on agent_conversations.role. Advisory only —
// the authoritative "live" generation is agents.current_conv_id.
const (
	ConvRoleHead       = "head"       // the current / chain-head generation
	ConvRoleGeneration = "generation" // a superseded past generation
)

// newAgentID mints a fresh, opaque, prefixed agent id. Panics only if the
// system CSPRNG fails — an identity the daemon cannot generate is fatal,
// same posture as the operator-token generator.
func newAgentID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("db: crypto/rand failed generating agent_id: " + err.Error())
	}
	return AgentIDPrefix + hex.EncodeToString(b[:])
}

// AllocateAgent mints a brand-new actor whose first (and current)
// conversation generation is convID, and links convID to it. Use it on the
// birth of a genuinely new agent — spawn, clone (a fork is a new actor), or
// the catch-all enrollment of a conv that talks to the daemon. For a
// rotation (reincarnate / /clear), the new conv joins the EXISTING agent via
// LinkConvToAgent + SetAgentCurrentConv instead.
//
// Errors if convID is already linked to an agent — callers that want
// idempotency should use EnsureAgentForConv.
func AllocateAgent(convID, via string) (string, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return "", errors.New("AllocateAgent: conv_id required")
	}
	d, err := Open()
	if err != nil {
		return "", err
	}
	tx, err := d.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	if existing, err := agentIDForConvTx(tx, convID); err != nil {
		return "", err
	} else if existing != "" {
		return "", fmt.Errorf("AllocateAgent: conv %s already belongs to agent %s", convID, existing)
	}
	agentID := newAgentID()
	if err := insertAgentTx(tx, agentID, convID, via, time.Now()); err != nil {
		return "", err
	}
	if err := linkConvTx(tx, convID, agentID, ConvRoleHead, via, time.Now()); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return agentID, nil
}

// EnsureAgentForConv is the idempotent dual-write workhorse: it returns the
// agent already owning convID, or — when convID is unlinked — allocates a
// fresh actor for it. created reports whether a new agent was minted.
//
// This is what every "a conv became an agent" path calls (the /v1 catch-all
// enrollment, spawn, clone). A rotated conv is linked by the rotation path
// BEFORE it ever self-enrolls, so by the time it reaches here it is already
// mapped and this is a no-op — it never splits a replacement generation off
// into its own actor.
func EnsureAgentForConv(convID, via string) (agentID string, created bool, err error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return "", false, errors.New("EnsureAgentForConv: conv_id required")
	}
	if existing, err := AgentIDForConv(convID); err != nil {
		return "", false, err
	} else if existing != "" {
		return existing, false, nil
	}
	agentID, err = AllocateAgent(convID, via)
	if err != nil {
		// Lost a race with a concurrent allocate/link — re-read and return
		// the winner rather than surfacing a spurious error.
		if existing, rerr := AgentIDForConv(convID); rerr == nil && existing != "" {
			return existing, false, nil
		}
		return "", false, err
	}
	return agentID, true, nil
}

// LinkConvToAgent maps an additional conversation generation onto an existing
// actor — the rotation primitive (reincarnate / /clear link the fresh conv to
// the predecessor's agent_id). INSERT OR IGNORE on the conv_id primary key:
// re-linking the same conv is a no-op, and a conv can never belong to two
// actors.
func LinkConvToAgent(convID, agentID, role, reason string) error {
	convID = strings.TrimSpace(convID)
	agentID = strings.TrimSpace(agentID)
	if convID == "" || agentID == "" {
		return errors.New("LinkConvToAgent: conv_id and agent_id required")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	return linkConvTx(d, convID, agentID, role, reason, time.Now())
}

// AgentIDForConv resolves a conversation generation to its owning actor, or
// "" when the conv is not (yet) known as an agent. This is the lookup the
// authorization path will route through to turn the request's current
// ConvID into the stable AgentID.
func AgentIDForConv(convID string) (string, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return "", nil
	}
	d, err := Open()
	if err != nil {
		return "", err
	}
	return agentIDForConvTx(d, convID)
}

// GetAgent returns the actor row for agentID, or nil when unknown.
func GetAgent(agentID string) (*Agent, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`SELECT agent_id, current_conv_id, created_at, created_via,
		retired_at, retired_by, retire_reason, pending_name, retired_by_agent
		FROM agents WHERE agent_id = ?`, agentID)
	a, err := scanAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

// CurrentConvForAgent returns the conv-id of an agent's CURRENT (head)
// generation — agents.current_conv_id — or "" if the agent is unknown. This is
// the conv the agent-keyed nudge drain delivers to (JOH-310): it follows the
// actor across reincarnate / /clear, so a message queued before a rotation
// still reaches the live generation. It is the non-transaction sibling of
// currentConvForAgentTx.
func CurrentConvForAgent(agentID string) (string, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return "", nil
	}
	d, err := Open()
	if err != nil {
		return "", err
	}
	var cur string
	err = d.QueryRow(`SELECT current_conv_id FROM agents WHERE agent_id = ?`, agentID).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return cur, err
}

// GetAgentByConv resolves a conversation generation straight to its actor
// row, or nil when the conv is not an agent.
func GetAgentByConv(convID string) (*Agent, error) {
	agentID, err := AgentIDForConv(convID)
	if err != nil || agentID == "" {
		return nil, err
	}
	return GetAgent(agentID)
}

// FindAgentsByIDPrefix returns every actor whose agent_id begins with prefix,
// oldest first. The selector resolver uses it to accept a (possibly shortened)
// stable agent_id — the canonical, rotation-immune way to name an agent. The
// caller decides 0 / 1 / many handling: a unique match resolves; several are
// surfaced as an ambiguity.
//
// The agent-id tag `agt_` contains a literal underscore, which is a LIKE
// wildcard, so the prefix is escaped (unlike FindConvIndexByPrefix, whose
// conv-ids are bare UUIDs with no LIKE-special characters).
func FindAgentsByIDPrefix(prefix string) ([]*Agent, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT agent_id, current_conv_id, created_at, created_via,
		retired_at, retired_by, retire_reason, pending_name, retired_by_agent
		FROM agents WHERE agent_id LIKE ? ESCAPE '\' ORDER BY created_at, rowid`, likeEscape(prefix)+"%")
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

// ConvsForAgent returns every conversation generation linked to agentID,
// oldest link first. The current generation is agents.current_conv_id.
func ConvsForAgent(agentID string) ([]string, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	// Order by rowid (link-insertion order), NOT linked_at. linked_at is stored
	// as RFC3339Nano, whose trimmed / variable-width fractional seconds make a
	// lexicographic string sort disagree with chronological order: a value that
	// lands exactly on a whole second formats with no fraction at all ("…43Z"),
	// and '.' < 'Z', so it sorts AFTER a same-second value that does have one
	// ("…43.0001Z") — and since the strings differ, the old rowid tiebreaker
	// never engaged. rowid is monotonic with insertion, which IS the link order:
	// generations are appended as they happen at runtime, and the v72 backfill
	// stamps every migrated row with the same linked_at (so it already leaned on
	// the rowid tiebreaker). See GenerationsForAgent + generations_test.go.
	rows, err := d.Query(`SELECT conv_id FROM agent_conversations
		WHERE agent_id = ? ORDER BY rowid`, agentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GenerationsForAgent returns every conversation generation linked to agentID
// as full AgentConversation rows (conv_id, role, reason, linked_at), oldest
// link first. The richer twin of ConvsForAgent — used by surfaces that need to
// annotate a generation with why/when it was linked (e.g. the dashboard's
// "Replaced generations" view, which renders each predecessor with its
// rotation reason and age). The current generation is agents.current_conv_id;
// callers wanting only the superseded predecessors filter that out.
func GenerationsForAgent(agentID string) ([]AgentConversation, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	// Oldest link first, ordered by rowid (link-insertion order) — NOT linked_at,
	// whose RFC3339Nano string sort is not chronological. See ConvsForAgent.
	rows, err := d.Query(`SELECT conv_id, role, reason, linked_at
		FROM agent_conversations WHERE agent_id = ? ORDER BY rowid`, agentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AgentConversation
	for rows.Next() {
		var (
			c        AgentConversation
			linkedAt string
		)
		c.AgentID = agentID
		if err := rows.Scan(&c.ConvID, &c.Role, &c.Reason, &linkedAt); err != nil {
			return nil, err
		}
		c.LinkedAt, _ = time.Parse(time.RFC3339Nano, linkedAt)
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetAgentCurrentConv advances an actor's live conversation pointer as a
// compare-and-swap: it only moves the pointer when it currently equals
// expectedOldConv. Returns true when the pointer moved. The CAS guard means
// two racing rotations cannot both advance the same actor from stale state —
// the second observes a mismatch (false) instead of silently clobbering.
//
// newConv MUST already be a linked generation of agentID (link it via
// LinkConvToAgent first) — the live pointer is, by definition, one of the
// actor's own generations, never an unrelated or unlinked conv. A newConv
// that is not linked to agentID is rejected (false, error). Pass
// expectedOldConv == "" to set the pointer unconditionally.
func SetAgentCurrentConv(agentID, expectedOldConv, newConv string) (bool, error) {
	agentID = strings.TrimSpace(agentID)
	newConv = strings.TrimSpace(newConv)
	if agentID == "" || newConv == "" {
		return false, errors.New("SetAgentCurrentConv: agent_id and new conv_id required")
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	owner, err := agentIDForConvTx(d, newConv)
	if err != nil {
		return false, err
	}
	if owner != agentID {
		return false, fmt.Errorf("SetAgentCurrentConv: conv %s is not a linked generation of agent %s (owner=%q)",
			newConv, agentID, owner)
	}
	var res sql.Result
	if expectedOldConv == "" {
		res, err = d.Exec(`UPDATE agents SET current_conv_id = ? WHERE agent_id = ?`,
			newConv, agentID)
	} else {
		res, err = d.Exec(`UPDATE agents SET current_conv_id = ?
			WHERE agent_id = ? AND current_conv_id = ?`,
			newConv, agentID, expectedOldConv)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetAgentPendingName records the actor's intended display name. A plain
// UPDATE — a no-op when the agent is unknown.
func SetAgentPendingName(agentID, name string) error {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return errors.New("SetAgentPendingName: agent_id required")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE agents SET pending_name = ? WHERE agent_id = ?`,
		strings.TrimSpace(name), agentID)
	return err
}

// SetAgentInitialSpawnConfig records the verbatim JSON snapshot of the spawn
// request an actor was born from onto agents.initial_spawn_config — the durable,
// agent-level "what was this spawned with" record. A plain UPDATE — a no-op when
// the agent is unknown. Written once at spawn enrollment; later lifecycle ops
// (rename, reincarnate, /clear) never touch it, so it stays the birth record.
// The column is write-only by design: tclaude stores it verbatim and never reads
// it back (resume reads live state), so there is no matching getter.
func SetAgentInitialSpawnConfig(agentID, cfg string) error {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return errors.New("SetAgentInitialSpawnConfig: agent_id required")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE agents SET initial_spawn_config = ? WHERE agent_id = ?`,
		cfg, agentID)
	return err
}

// SetAgentProcessCommand binds a spawned actor to the deterministic process
// command that created it. The database's partial unique index rejects a
// second actor for the same non-empty command id.
func SetAgentProcessCommand(agentID, commandID string) error {
	agentID = strings.TrimSpace(agentID)
	commandID = strings.TrimSpace(commandID)
	if agentID == "" || commandID == "" {
		return errors.New("SetAgentProcessCommand: agent_id and command_id required")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE agents SET process_command_id = ? WHERE agent_id = ?`, commandID, agentID)
	return err
}

func AgentForProcessCommand(commandID string) (*Agent, error) {
	commandID = strings.TrimSpace(commandID)
	if commandID == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var agentID string
	err = d.QueryRow(`SELECT agent_id FROM agents WHERE process_command_id = ?`, commandID).Scan(&agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return GetAgent(agentID)
}

func ClearAgentProcessCommandForConv(convID string) error {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return nil
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE agents SET process_command_id = '' WHERE agent_id = (
		SELECT agent_id FROM agent_conversations WHERE conv_id = ?
	)`, convID)
	return err
}

// RetireAgentByID demotes an active actor: it sets retired_at so the agent
// drops off every live surface, leaving its conversation generations intact.
// Returns false (no error) when the agent was not active, so a repeated
// cleanup is idempotent.
func RetireAgentByID(agentID, by, reason string) (bool, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return false, errors.New("RetireAgentByID: agent_id required")
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	// Derive the retirer's stable agent_id from `by` and dual-write it to the
	// retired_by_agent companion (JOH-306). `by` is either a literal ("human",
	// "system:export-clone") or the retirer's conv-id (enrollmentActor). The
	// same agent_conversations lookup the v78 backfill uses resolves a conv-id
	// to its owning actor; a literal matches no row, so the companion stays ''.
	// Deriving here (rather than threading a separate param) keeps
	// retired_by_agent consistent with retired_by by construction, and makes
	// freshly-written rows agree with backfilled ones.
	//
	// Best-effort: a failed/empty resolution must never block the retire — the
	// companion is an audit decoration, not a precondition. On any error or a
	// non-actor `by`, the companion stays '' and the display falls back to the
	// raw retired_by value.
	byAgent, _ := agentIDForConvTx(d, by)
	res, err := d.Exec(`UPDATE agents
		SET retired_at = ?, retired_by = ?, retire_reason = ?, retired_by_agent = ?
		WHERE agent_id = ? AND retired_at = ''`,
		time.Now().Format(time.RFC3339Nano), by, reason, byAgent, agentID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReinstateAgentByID clears the retired flag, returning a retired actor to
// active status. Returns false (no error) when the agent was not retired.
//
// Reinstating also revives the agent's cancelled nudge queue: undelivered
// messages the reaper's orphan sweep abandoned while the agent was retired
// (CancelAgentMessageNudge) get their cancellation cleared, so queued mail
// resumes delivery once the agent comes back online. Both updates run in one
// transaction so a reinstated agent can never be observed with a
// still-cancelled queue.
func ReinstateAgentByID(agentID string) (bool, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return false, errors.New("ReinstateAgentByID: agent_id required")
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
	res, err := tx.Exec(`UPDATE agents
		SET retired_at = '', retired_by = '', retire_reason = '', retired_by_agent = ''
		WHERE agent_id = ? AND retired_at != ''`, agentID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, tx.Commit()
	}
	// Head-following mail is keyed by to_agent; prev-gen-pinned and non-actor
	// mail cancelled via the conv's owning actor is keyed by to_conv, so the
	// revive matches either the stable agent_id or any of its generations.
	if _, err := tx.Exec(`UPDATE agent_messages
		SET nudge_cancelled_at = '', nudge_cancel_reason = ''
		WHERE nudge_cancelled_at != '' AND delivered_at = '' AND read_at = ''
		  AND (to_agent = ? OR to_conv IN (
			SELECT conv_id FROM agent_conversations WHERE agent_id = ?))`,
		agentID, agentID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ListActiveAgents / ListRetiredAgents return the actor-level rosters — the
// canonical roster surfaces (the dashboard, power/focus, mailbox, retired
// cleanup) read these (JOH-26 PR3b). A reincarnate / Claude Code /clear
// predecessor is a past generation of an ACTIVE actor, so it never appears on
// the retired roster; only an actor a human explicitly retired (its retired_at
// dual-written by db.RetireAgent) does. Active reports one row per live actor
// (its current_conv).
func ListActiveAgents() ([]*Agent, error) { return listAgents(`retired_at = ''`) }

// ListRetiredAgents returns the retired actors (the dashboard reinstate
// candidates).
func ListRetiredAgents() ([]*Agent, error) { return listAgents(`retired_at != ''`) }

// ListAgentConvIDs returns every conversation generation known to the actor
// layer — the full set of agent_conversations.conv_id across all actors (active,
// retired, and superseded predecessors alike). Roster surfaces use it to exclude
// EVERY agent generation from the "plain conversations" list: the actor rosters
// expose only each actor's current conv, so without this a predecessor
// generation would leak in as a promotion candidate.
func ListAgentConvIDs() (map[string]struct{}, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT conv_id FROM agent_conversations`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]struct{}{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out[c] = struct{}{}
	}
	return out, rows.Err()
}

func listAgents(where string) ([]*Agent, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT agent_id, current_conv_id, created_at, created_via,
		retired_at, retired_by, retire_reason, pending_name, retired_by_agent
		FROM agents WHERE ` + where + ` ORDER BY created_at, rowid`)
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

// Agent states — the actor-table tri-state that replaced the removed
// agent_enrollment tri-state (JOH-26 PR3c). A conversation is an agent exactly
// when it has an agent_conversations row; the owning actor's retired_at then
// decides active vs retired. A predecessor generation resolves to its (live)
// actor, so it reads AgentStateActive — a past generation of an active agent,
// never a standalone retired entry (the actor-level Retired-tray behaviour PR3b
// shipped).
const (
	AgentStateNone    = "none"    // not an agent
	AgentStateActive  = "active"  // live actor
	AgentStateRetired = "retired" // actor explicitly retired
)

// AgentState returns AgentStateNone / Active / Retired for convID — the cheap
// probe the read paths use to decide whether a conv belongs on the agent
// roster. The actor-table successor to the removed db.EnrollmentState.
func AgentState(convID string) (string, error) {
	a, err := GetAgentByConv(convID)
	if err != nil {
		return "", err
	}
	switch {
	case a == nil:
		return AgentStateNone, nil
	case a.Active():
		return AgentStateActive, nil
	default:
		return AgentStateRetired, nil
	}
}

// IsLiveAgentConv reports whether convID is the CURRENT generation of an active
// (non-retired) actor — a live agent addressable at this exact conv. A
// superseded predecessor generation (resolves to the actor but is not its
// current_conv) and a retired actor both return false. The retire path gates on
// this so a stale / predecessor handle can never demote the live actor; in
// normal operation every caller already resolves to the current generation
// (agent.ResolveSelector redirects a predecessor forward to the chain head), so
// this is the explicit, defensive twin of that invariant.
func IsLiveAgentConv(convID string) (bool, error) {
	a, err := GetAgentByConv(convID)
	if err != nil || a == nil {
		return false, err
	}
	return a.Active() && a.CurrentConvID == convID, nil
}

// PromoteAgent makes convID's actor an active agent — the explicit, deliberate
// path behind the dashboard "promote" / "reinstate" buttons and the
// `tclaude agent promote` CLI. EnsureAgentForConv mints an actor for a
// brand-new conv; a retired actor is reinstated. Returns the prior AgentState
// so the caller can tell a promote ("none") from a reinstate ("retired") and
// report a no-op ("active") honestly. Single source as of JOH-26 PR3c — the
// agents table is the only roster, so there is no second write to diverge from.
func PromoteAgent(convID, via string) (prior string, err error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return "", errors.New("PromoteAgent: conv_id required")
	}
	prior, err = AgentState(convID)
	if err != nil {
		return "", err
	}
	agentID, _, err := EnsureAgentForConv(convID, via)
	if err != nil {
		return prior, err
	}
	if prior == AgentStateRetired {
		if _, err := ReinstateAgentByID(agentID); err != nil {
			return prior, err
		}
	}
	return prior, nil
}

// RetireAgent demotes convID's actor to a plain conversation: it sets the
// actor's retired_at so the agent drops off every live surface. The
// conversation data itself is untouched — this is the non-destructive half of
// cleanup. Callers must first revoke the conv's group memberships and
// permission grants; RetireAgent only flips the bit. Returns false (no error)
// when convID was not an active agent, so a repeated cleanup is idempotent.
//
// Conv-keyed convenience over RetireAgentByID: it resolves convID to its stable
// actor and retires that. A predecessor generation resolves to its live actor —
// but in practice retire is only ever issued against an actor's current
// generation (the resolved selector), so this retires exactly the intended
// actor.
func RetireAgent(convID, by, reason string) (bool, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return false, errors.New("RetireAgent: conv_id required")
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return false, err
	}
	if agentID == "" {
		return false, nil
	}
	return RetireAgentByID(agentID, by, reason)
}

// ReinstateAgent clears the retired flag on convID's actor, returning a retired
// agent to active status. Its groups and grants do not come back — retire
// stripped those — so a reinstated agent starts fresh. Returns false (no error)
// when convID's actor was not retired. Conv-keyed convenience over
// ReinstateAgentByID.
func ReinstateAgent(convID string) (bool, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return false, errors.New("ReinstateAgent: conv_id required")
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return false, err
	}
	if agentID == "" {
		return false, nil
	}
	return ReinstateAgentByID(agentID)
}

// PendingNamesByConv returns conv_id → pending_name for every actor that
// recorded a non-empty spawn-time name, keyed on the actor's CURRENT conv. It
// is the bulk display-fallback counterpart to GetAgentByConv(...).PendingName,
// for listing surfaces that need the designated agent name before the agent's
// own title write has landed (e.g. a Codex agent) without a per-row query.
// Actors with no pending name are simply absent from the map. Actor-keyed since
// JOH-26 PR3c, so the fallback survives conv rotations.
func PendingNamesByConv() (map[string]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT current_conv_id, pending_name FROM agents
		WHERE pending_name IS NOT NULL AND pending_name != ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]string)
	for rows.Next() {
		var convID, name string
		if err := rows.Scan(&convID, &name); err != nil {
			return nil, err
		}
		out[convID] = name
	}
	return out, rows.Err()
}

// --- transaction-scoped helpers (shared by the runtime API and the
// migration backfill, which must operate on the migration's *sql.DB rather
// than re-entering Open()) ---

// dbExecQuerier is the minimal surface the tx-scoped helpers need, satisfied
// by both *sql.DB and *sql.Tx.
type dbExecQuerier interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// ensureAgentForConvTx is the tx-scoped twin of EnsureAgentForConv: it
// returns the actor owning convID, allocating one (current_conv_id = convID)
// when convID is unlinked. Used by MigrateAgentIdentity, which already runs
// inside a transaction and must not re-enter Open(). Unlike the migration
// backfill it does not carry enrollment facts — the rotation caller knows
// the actor already exists in practice; this is the defensive allocate for
// the window before the conv-layer is fully populated.
func ensureAgentForConvTx(tx dbExecQuerier, convID, via string) (string, error) {
	if existing, err := agentIDForConvTx(tx, convID); err != nil {
		return "", err
	} else if existing != "" {
		return existing, nil
	}
	agentID := newAgentID()
	if err := insertAgentTx(tx, agentID, convID, via, time.Now()); err != nil {
		return "", err
	}
	if err := linkConvTx(tx, convID, agentID, ConvRoleHead, via, time.Now()); err != nil {
		return "", err
	}
	return agentID, nil
}

func agentIDForConvTx(q dbExecQuerier, convID string) (string, error) {
	var agentID string
	err := q.QueryRow(`SELECT agent_id FROM agent_conversations WHERE conv_id = ?`, convID).Scan(&agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return agentID, nil
}

func insertAgentTx(q dbExecQuerier, agentID, convID, via string, now time.Time) error {
	_, err := q.Exec(`INSERT INTO agents
		(agent_id, current_conv_id, created_at, created_via) VALUES (?, ?, ?, ?)`,
		agentID, convID, now.Format(time.RFC3339Nano), via)
	return err
}

// linkConvTx maps convID onto agentID. Conflict-aware: idempotent when convID
// is already linked to THIS actor, but a hard error when it is linked to a
// DIFFERENT one — a conversation generation belongs to exactly one actor, and
// silently ignoring a cross-actor relink (the old INSERT OR IGNORE) would let
// agents.current_conv_id drift onto a conv owned by someone else. The check
// is a SELECT before the INSERT, so on the conflict path it mutates nothing
// and leaves the surrounding transaction usable.
func linkConvTx(q dbExecQuerier, convID, agentID, role, reason string, now time.Time) error {
	existing, err := agentIDForConvTx(q, convID)
	if err != nil {
		return err
	}
	if existing == agentID {
		return nil // already ours — idempotent
	}
	if existing != "" {
		return fmt.Errorf("linkConvToAgent: conv %s already belongs to agent %s (refusing to relink to %s)",
			convID, existing, agentID)
	}
	if _, err = q.Exec(`INSERT INTO agent_conversations
		(conv_id, agent_id, role, reason, linked_at) VALUES (?, ?, ?, ?, ?)`,
		convID, agentID, role, reason, now.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	// Fill owner rows written before this conv enrolled (a sessions row predates
	// the hook that links the agent). Insert-time dual-write covers the rest.
	return propagateAgentCompanions(q, convID, agentID)
}

// advanceAgentToNewConv is the rotation primitive: it links newConv as the
// fresh head generation of agentID and advances the live pointer from oldConv
// to newConv, demoting oldConv and carrying the display name onto the actor.
//
// It is deliberately best-effort and NON-ABORTING in its skip cases, so a
// caller can run it inside a larger transaction (reincarnate / /clear) without
// the dual-write ever rolling back the legacy work:
//
//   - returns (false, nil) — a clean skip — when newConv already belongs to a
//     DIFFERENT actor (nothing is mutated), or when the CAS finds oldConv is
//     no longer the live head (newConv is linked as a GENERATION so the
//     successor is still owned by the actor, but no head/pointer change is
//     made — there is never a second advisory head).
//   - returns (true, nil) when the pointer moved; only then is newConv
//     promoted to head, oldConv demoted, and the name carried.
//   - returns (_, err) only on a real DB error.
//
// Role changes are sequenced strictly AFTER the compare-and-swap confirms the
// pointer advanced, so a missed CAS can never leave the actor with two head
// rows, and a same-actor pre-linked successor is correctly promoted to head on
// a successful advance.
func advanceAgentToNewConv(tx dbExecQuerier, agentID, oldConv, newConv, reason, carriedName string, now time.Time) (bool, error) {
	existing, err := agentIDForConvTx(tx, newConv)
	if err != nil {
		return false, err
	}
	if existing != "" && existing != agentID {
		return false, nil // successor already owned by another actor — skip cleanly, mutate nothing
	}
	// Link the successor as a GENERATION first. The head role is conferred
	// only after the CAS below confirms the advance, so a missed CAS leaves a
	// plain generation link (not a phantom second head).
	if existing == "" {
		if _, err := tx.Exec(`INSERT INTO agent_conversations
			(conv_id, agent_id, role, reason, linked_at) VALUES (?, ?, ?, ?, ?)`,
			newConv, agentID, ConvRoleGeneration, reason, now.Format(time.RFC3339Nano)); err != nil {
			return false, err
		}
	}
	// newConv now belongs to agentID; fill any owner rows already keyed on it
	// (a sessions row for the successor can be registered before this link).
	if err := propagateAgentCompanions(tx, newConv, agentID); err != nil {
		return false, err
	}
	res, err := tx.Exec(`UPDATE agents SET current_conv_id = ?
		WHERE agent_id = ? AND current_conv_id = ?`, newConv, agentID, oldConv)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil // oldConv was not the live head — leave the pointer + roles alone
	}
	// The pointer advanced: promote newConv to head (covers a same-actor
	// pre-linked generation too), demote oldConv, carry the display name.
	if _, err := tx.Exec(`UPDATE agent_conversations SET role = ?
		WHERE conv_id = ? AND agent_id = ?`, ConvRoleHead, newConv, agentID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE agent_conversations SET role = ?
		WHERE conv_id = ? AND agent_id = ?`, ConvRoleGeneration, oldConv, agentID); err != nil {
		return false, err
	}
	if carriedName != "" {
		if _, err := tx.Exec(`UPDATE agents SET pending_name = ? WHERE agent_id = ?`,
			carriedName, agentID); err != nil {
			return false, err
		}
	}
	return true, nil
}

// absorbBareSuccessorActorTx resolves the reincarnate ordering hazard: a
// reincarnate successor is spawned as a fresh tclaude session, and its
// session-start hook self-registers its OWN actor for newConv
// (EnsureAgentForConv) BEFORE the daemon's rotation links newConv to the
// predecessor's actor. Left alone, advanceAgentToNewConv would clean-skip
// (newConv is owned by a different actor) and the agent's identity — group
// memberships, ownerships, permissions — would NOT carry across the
// reincarnation. (A /clear does not hit this: its successor is an in-process
// rotation whose session-start is a transition and is not self-registered.)
//
// When that self-registered actor is BARE — it holds no group membership /
// ownership / permission / sudo / notify / cron identity and newConv is its only
// generation — it is safe to absorb: delete it (ON DELETE CASCADE frees newConv)
// so the caller can relink newConv onto keepAgentID and advance the live
// pointer. A self-registered actor that has somehow already accrued real
// identity is left untouched (returns false) — the caller then observes the
// pointer did not advance and treats the rotation as failed rather than
// destroying state.
//
// Returns true when an actor was absorbed (newConv is now unlinked, ready to
// relink to keepAgentID).
func absorbBareSuccessorActorTx(tx dbExecQuerier, keepAgentID, newConv string) (bool, error) {
	newOwner, err := agentIDForConvTx(tx, newConv)
	if err != nil {
		return false, err
	}
	if newOwner == "" || newOwner == keepAgentID {
		return false, nil // newConv unlinked, or already ours — nothing to absorb
	}
	// The successor's actor must own NOTHING but newConv to be safely absorbed.
	// Any identity row, or any OTHER conversation generation, means it is not a
	// fresh self-enroll and must not be destroyed.
	guards := []struct {
		q    string
		args []any
	}{
		{`SELECT COUNT(*) FROM agent_group_members WHERE agent_id = ?`, []any{newOwner}},
		{`SELECT COUNT(*) FROM agent_group_owners WHERE agent_id = ?`, []any{newOwner}},
		{`SELECT COUNT(*) FROM agent_permissions WHERE agent_id = ?`, []any{newOwner}},
		{`SELECT COUNT(*) FROM agent_sudo_grants WHERE agent_id = ?`, []any{newOwner}},
		{`SELECT COUNT(*) FROM agent_notify_prefs WHERE agent_id = ?`, []any{newOwner}},
		{`SELECT COUNT(*) FROM agent_cron_jobs WHERE owner_agent = ? OR target_agent = ?`, []any{newOwner, newOwner}},
		// agent-keyed rate-limit history (JOH-26 PR3a): an actor that has spawned
		// or was cloned-from has mattered — these rows have no FK to agents, so
		// absorbing the actor would orphan them.
		{`SELECT COUNT(*) FROM agent_spawn_history WHERE spawner_agent_id = ?`, []any{newOwner}},
		{`SELECT COUNT(*) FROM agent_clone_history WHERE source_agent_id = ?`, []any{newOwner}},
		{`SELECT COUNT(*) FROM agent_conversations WHERE agent_id = ? AND conv_id != ?`, []any{newOwner, newConv}},
	}
	for _, g := range guards {
		var n int
		if err := tx.QueryRow(g.q, g.args...).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return false, nil // has real identity / other generations — don't absorb
		}
	}
	// Bare: delete the actor. The agent_conversations FK cascades, dropping its
	// newConv link, so newConv becomes unlinked and free to relink.
	if _, err := tx.Exec(`DELETE FROM agents WHERE agent_id = ?`, newOwner); err != nil {
		return false, err
	}
	return true, nil
}

func scanAgent(s rowScanner) (*Agent, error) {
	var a Agent
	var createdAt, retiredAt string
	if err := s.Scan(&a.AgentID, &a.CurrentConvID, &createdAt, &a.CreatedVia,
		&retiredAt, &a.RetiredBy, &a.RetireReason, &a.PendingName, &a.RetiredByAgent); err != nil {
		return nil, err
	}
	a.CreatedAt = parseTimeOrZero(createdAt)
	a.RetiredAt = parseTimeOrZero(retiredAt)
	return &a, nil
}
