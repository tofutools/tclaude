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

// agentIDPrefix tags a stable agent id so it is unmistakable next to a
// conv-id (a bare UUID) in logs and the DB. 16 random bytes ≈ 128 bits of
// entropy — collision-free for this single-operator tool.
const agentIDPrefix = "agt_"

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
	RetireReason  string
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
	return agentIDPrefix + hex.EncodeToString(b[:])
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
		retired_at, retired_by, retire_reason, pending_name
		FROM agents WHERE agent_id = ?`, agentID)
	a, err := scanAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
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
	rows, err := d.Query(`SELECT conv_id FROM agent_conversations
		WHERE agent_id = ? ORDER BY linked_at, rowid`, agentID)
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

// SetAgentCurrentConv advances an actor's live conversation pointer as a
// compare-and-swap: it only moves the pointer when it currently equals
// expectedOldConv. Returns true when the pointer moved. The CAS guard means
// two racing rotations cannot both advance the same actor from stale state —
// the second observes a mismatch (false) instead of silently clobbering.
//
// Pass expectedOldConv == "" to set the pointer unconditionally (the
// allocate path, where there is no prior generation to guard against).
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
	res, err := d.Exec(`UPDATE agents
		SET retired_at = ?, retired_by = ?, retire_reason = ?
		WHERE agent_id = ? AND retired_at = ''`,
		time.Now().Format(time.RFC3339Nano), by, reason, agentID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReinstateAgentByID clears the retired flag, returning a retired actor to
// active status. Returns false (no error) when the agent was not retired.
func ReinstateAgentByID(agentID string) (bool, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return false, errors.New("ReinstateAgentByID: agent_id required")
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE agents
		SET retired_at = '', retired_by = '', retire_reason = ''
		WHERE agent_id = ? AND retired_at != ''`, agentID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListActiveAgents2 / ListRetiredAgents2 return the actor-level rosters. The
// `2` suffix is temporary scaffolding: they live alongside the conv-keyed
// agent_enrollment list functions until the roster surfaces cut over to
// agent_id, at which point the enrollment ones retire and these lose the
// suffix.
func ListActiveAgents2() ([]*Agent, error) { return listAgents(`retired_at = ''`) }

// ListRetiredAgents2 returns the retired actors (the dashboard reinstate
// candidates).
func ListRetiredAgents2() ([]*Agent, error) { return listAgents(`retired_at != ''`) }

func listAgents(where string) ([]*Agent, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT agent_id, current_conv_id, created_at, created_via,
		retired_at, retired_by, retire_reason, pending_name
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

func linkConvTx(q dbExecQuerier, convID, agentID, role, reason string, now time.Time) error {
	_, err := q.Exec(`INSERT OR IGNORE INTO agent_conversations
		(conv_id, agent_id, role, reason, linked_at) VALUES (?, ?, ?, ?, ?)`,
		convID, agentID, role, reason, now.Format(time.RFC3339Nano))
	return err
}

func scanAgent(s rowScanner) (*Agent, error) {
	var a Agent
	var createdAt, retiredAt string
	if err := s.Scan(&a.AgentID, &a.CurrentConvID, &createdAt, &a.CreatedVia,
		&retiredAt, &a.RetiredBy, &a.RetireReason, &a.PendingName); err != nil {
		return nil, err
	}
	a.CreatedAt = parseTimeOrZero(createdAt)
	a.RetiredAt = parseTimeOrZero(retiredAt)
	return &a, nil
}
