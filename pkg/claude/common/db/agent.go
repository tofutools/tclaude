package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AgentGroup is a row in agent_groups.
type AgentGroup struct {
	ID               int64
	Name             string
	Descr            string
	DefaultCwd       string // pre-filled cwd for agents spawned into this group; "" = none
	DefaultContext   string // shared startup context delivered to the inbox of agents spawned into this group; "" = none
	DefaultProfile   string // current name of the stable spawn-profile reference whose launch fields fill blanks server-side; "" = none
	SandboxProfile   string // current name of the stable sandbox-profile assignment; "" = none
	SandboxProfileID int64  `json:"-"`
	MaxMembers       int    // hard cap on member count; a spawn that would exceed it is refused. 0 = unlimited
	NotifyEnabled    bool   // OS notifications for member agents; false mutes the whole group (a per-agent 'on' pref still overrides)
	RemoteControl    *bool  // remote-control policy for agents spawned into this group; tri-state: nil = inherit (defer to the spawn profile), false = actively deny (force off), true = actively opt-in (force on). Overrides the profile default (JOH-262)
	// Mission and SourceTemplate are deployment provenance (JOH-245): what a
	// task force was deployed against (a free-text topic or Linear link) and the
	// template it was instantiated/deployed from (surfaced as its current name
	// through a stable template id). "" for a group not created from a template
	// (the dashboard reads both-blank as "not a deployed force").
	Mission        string
	SourceTemplate string
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
}

// IsArchived reports whether the group has been soft-deleted via
// `groups archive`. Archived groups are hidden from default listings
// and reject mutating operations (member.add / member.remove /
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

// Permission-override effects — the value stored in
// agent_permissions.effect (schema v38+). A row with effect=grant ADDS
// the slug on top of the config.json defaults; effect=deny SUBTRACTS
// it, overriding a default grant for that one conv. The absence of a
// row is the third, neutral state: "inherit the default."
const (
	PermEffectGrant = "grant"
	PermEffectDeny  = "deny"
)

// AgentPermission is a row in agent_permissions — a per-conv permission
// override. Lives in SQLite (rather than ~/.tclaude/config.json) so the
// daemon can grant/revoke without JSON-file rewrites. DefaultPermissions
// for *all* agents still live in config.json, since those describe
// baseline trust the human curates explicitly.
//
// Effect is "grant" or "deny" (see PermEffectGrant / PermEffectDeny).
type AgentPermission struct {
	ConvID    string
	Slug      string
	Effect    string
	GrantedAt time.Time
	GrantedBy string
}

// HasAgentPermissionRow reports whether (convID, slug) is GRANTED in the
// agent_permissions table. A deny-effect row reads as false here — the
// daemon's per-agent override lookup wants "does an additive grant
// exist," and a deny is the opposite. Errors propagate so the caller can
// refuse rather than silently allow. Callers needing the full tri-state
// should use AgentPermissionOverride instead.
func HasAgentPermissionRow(convID, slug string) (bool, error) {
	db, err := Open()
	if err != nil {
		return false, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return false, err
	}
	if agentID == "" {
		return false, nil
	}
	var n int
	err = db.QueryRow(`SELECT COUNT(*) FROM agent_permissions WHERE agent_id = ? AND slug = ? AND effect = ?`,
		agentID, slug, PermEffectGrant).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// AgentPermissionOverride returns the tri-state per-conv override for
// (convID, slug): effect is "grant" or "deny" with ok=true when a row
// exists, or ("", false) when there is no override and the slug falls
// through to the config defaults.
func AgentPermissionOverride(convID, slug string) (effect string, ok bool, err error) {
	db, err := Open()
	if err != nil {
		return "", false, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return "", false, err
	}
	if agentID == "" {
		return "", false, nil
	}
	err = db.QueryRow(`SELECT effect FROM agent_permissions WHERE agent_id = ? AND slug = ?`,
		agentID, slug).Scan(&effect)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return effect, true, nil
}

// ListAgentPermissionsForConv returns every GRANTED slug for this conv,
// alphabetised. Deny-effect rows are excluded — this preserves the
// historical "additive grants" semantics the CLI / dashboard grants
// view relies on. Returns an empty slice (not nil) for a conv with no
// grants.
func ListAgentPermissionsForConv(convID string) ([]string, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return nil, err
	}
	if agentID == "" {
		return []string{}, nil
	}
	rows, err := db.Query(`SELECT slug FROM agent_permissions WHERE agent_id = ? AND effect = ? ORDER BY slug`,
		agentID, PermEffectGrant)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListAgentPermissionOverridesForConv returns the full tri-state
// override map for this conv — slug → "grant" | "deny". Convs with no
// overrides return an empty (non-nil) map. Used by the dashboard's
// permanent-permission editor to pre-populate the modal.
func ListAgentPermissionOverridesForConv(convID string) (map[string]string, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return nil, err
	}
	if agentID == "" {
		return map[string]string{}, nil
	}
	rows, err := db.Query(`SELECT slug, effect FROM agent_permissions WHERE agent_id = ? ORDER BY slug`, agentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var slug, effect string
		if err := rows.Scan(&slug, &effect); err != nil {
			return nil, err
		}
		out[slug] = effect
	}
	return out, rows.Err()
}

// ListAllAgentPermissions returns the full GRANT table, grouped by
// conv-id. Deny-effect rows are excluded — see ListAgentPermissionsForConv.
// Convs with no grants are absent from the map. Used by the dashboard /
// permissions ls view.
func ListAllAgentPermissions() (map[string][]string, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	// Keyed by the actor's CURRENT conv for display (the dashboard renders per
	// live conv); the table is agent-keyed, so resolve through agents.
	rows, err := db.Query(`SELECT ag.current_conv_id, p.slug
		FROM agent_permissions p JOIN agents ag ON ag.agent_id = p.agent_id
		WHERE p.effect = ? ORDER BY ag.current_conv_id, p.slug`,
		PermEffectGrant)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var c, s string
		if err := rows.Scan(&c, &s); err != nil {
			return nil, err
		}
		out[c] = append(out[c], s)
	}
	return out, rows.Err()
}

// ListAllAgentPermissionOverrides returns every per-conv override
// (grant AND deny), nested conv-id → slug → effect. Convs with no
// overrides are absent. Used by the dashboard snapshot so the per-agent
// editor can render the current state without a round-trip per agent.
func ListAllAgentPermissionOverrides() (map[string]map[string]string, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT ag.current_conv_id, p.slug, p.effect
		FROM agent_permissions p JOIN agents ag ON ag.agent_id = p.agent_id
		ORDER BY ag.current_conv_id, p.slug`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]map[string]string{}
	for rows.Next() {
		var c, s, e string
		if err := rows.Scan(&c, &s, &e); err != nil {
			return nil, err
		}
		if out[c] == nil {
			out[c] = map[string]string{}
		}
		out[c][s] = e
	}
	return out, rows.Err()
}

// GrantAgentPermission writes a grant-effect override for (convID, slug).
// Idempotent, and an UPSERT — granting a slug that currently carries a
// deny override flips it back to grant. grantedBy is informational
// ("<human>" or a granter's conv-id); empty is fine.
func GrantAgentPermission(convID, slug, grantedBy string) error {
	return SetAgentPermissionOverride(convID, slug, PermEffectGrant, grantedBy)
}

// SetAgentPermissionOverride writes the tri-state override row for
// (convID, slug) with the given effect ("grant" or "deny"). UPSERT:
// re-running with a different effect flips the row. To return the slug
// to its default, use RevokeAgentPermission (which deletes the row).
func SetAgentPermissionOverride(convID, slug, effect, grantedBy string) error {
	if effect != PermEffectGrant && effect != PermEffectDeny {
		return fmt.Errorf("invalid permission effect %q (want %q or %q)", effect, PermEffectGrant, PermEffectDeny)
	}
	db, err := Open()
	if err != nil {
		return err
	}
	// Holding a permission override (grant or deny) makes the conv an agent —
	// a deny is still per-agent permission config. EnsureAgentForConv mints /
	// links the stable actor; we then key the override on agent_id (JOH-26) so
	// it survives conv rotations without a rekey.
	if _, _, err := EnsureAgentForConv(convID, "grant"); err != nil {
		return err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return err
	}
	if agentID == "" {
		return fmt.Errorf("SetAgentPermissionOverride: no actor for conv %s", convID)
	}
	_, err = db.Exec(`INSERT INTO agent_permissions
		(agent_id, slug, effect, granted_at, granted_by)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(agent_id, slug) DO UPDATE SET
			effect     = excluded.effect,
			granted_at = excluded.granted_at,
			granted_by = excluded.granted_by`,
		agentID, slug, effect, time.Now().Format(time.RFC3339Nano), grantedBy)
	return err
}

// RevokeAgentPermission removes a single (convID, slug). Idempotent.
// Returns the number of rows deleted (0 if there was nothing to remove).
func RevokeAgentPermission(convID, slug string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return 0, err
	}
	if agentID == "" {
		return 0, nil
	}
	res, err := db.Exec(`DELETE FROM agent_permissions WHERE agent_id = ? AND slug = ?`, agentID, slug)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RevokeAllAgentPermissionsForConv drops every per-conv permission
// row for convID. Bulk cleanup variant for the conv-delete path.
// Idempotent — returns the number of rows removed.
func RevokeAllAgentPermissionsForConv(convID string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return 0, err
	}
	if agentID == "" {
		return 0, nil
	}
	res, err := db.Exec(`DELETE FROM agent_permissions WHERE agent_id = ?`, agentID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ReincarnationHandoffSubject is the subject the reincarnate path stamps
// on the handoff message it queues from a retiring predecessor to its live
// successor. It is a single source of truth shared by the writer
// (agentd.reincarnate) and the firehose filter (MailboxFilter.where), which
// carves these rows out of the retired-agent exclusion so the handoff still
// shows in "All agent messages" even though its sender is now retired.
const ReincarnationHandoffSubject = "reincarnation handoff"

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
}

// CreateAgentGroup inserts a new group. Returns the new group's ID.
func CreateAgentGroup(name, descr string) (int64, error) {
	return CreateAgentGroupWithParent(name, descr, "")
}

// CreateAgentGroupWithParent inserts a new group, optionally nested under an
// existing parent group. parentName == "" creates a top-level group.
func CreateAgentGroupWithParent(name, descr, parentName string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var parentID sql.NullInt64
	if parentName = strings.TrimSpace(parentName); parentName != "" {
		if parentName == name {
			return 0, ErrGroupParentCycle
		}
		if err := tx.QueryRow(`SELECT id FROM agent_groups WHERE name = ?`, parentName).Scan(&parentID.Int64); errors.Is(err, sql.ErrNoRows) {
			return 0, ErrGroupParentNotFound
		} else if err != nil {
			return 0, err
		}
		parentID.Valid = true
	}

	res, err := tx.Exec(`INSERT INTO agent_groups (name, descr, created_at, parent_id) VALUES (?, ?, ?, ?)`,
		name, descr, time.Now().Format(time.RFC3339Nano), parentID)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// CreateAgentGroupFrom inserts a new group named `name` that carries
// every configurable setting from `src` — descr, default cwd / context
// / profile, the max-members cap and the notify switch. created_at is
// stamped fresh and the new group comes up active (archived_at left
// zero) regardless of src's archived state, so cloning an archived
// group yields a live one. Returns the new group's ID.
//
// This is the full-fidelity sibling of CreateAgentGroup (which sets only
// name + descr): group-clone needs every column copied, not just descr,
// so a cloned group is configured identically to its source.
func CreateAgentGroupFrom(name string, src AgentGroup) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	defaultProfileID, err := registryIDByNameDB(db, "spawn_profiles", src.DefaultProfile)
	if err != nil {
		return 0, err
	}
	sourceTemplateID := sql.NullInt64{Int64: src.SourceTemplateID, Valid: src.SourceTemplateID > 0}
	if !sourceTemplateID.Valid {
		sourceTemplateID, err = registryIDByNameDB(db, "group_templates", src.SourceTemplate)
		if err != nil {
			return 0, err
		}
	}
	sandboxProfileID := sql.NullInt64{Int64: src.SandboxProfileID, Valid: src.SandboxProfileID > 0}
	if !sandboxProfileID.Valid {
		sandboxProfileID, err = registryIDByNameDB(db, "sandbox_profiles", src.SandboxProfile)
		if err != nil {
			return 0, err
		}
	}
	res, err := db.Exec(`
		INSERT INTO agent_groups
			(name, descr, default_cwd, default_context, default_profile, default_profile_id,
			 sandbox_profile, sandbox_profile_id, max_members, notify_enabled, remote_control,
			 mission, source_template, source_template_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		name, src.Descr, src.DefaultCwd, src.DefaultContext, src.DefaultProfile, defaultProfileID,
		src.SandboxProfile, sandboxProfileID, src.MaxMembers, src.NotifyEnabled, boolPtrToNull(src.RemoteControl),
		src.Mission, src.SourceTemplate, sourceTemplateID,
		time.Now().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetAgentGroupDescr sets (or, with descr == "", clears) the group's
// own one-line description — the text shown next to the group name on
// the dashboard, distinct from any per-member descr. Returns the
// number of rows affected — 0 means no group by that name, so the
// caller can answer 404.
func SetAgentGroupDescr(name, descr string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups SET descr = ? WHERE name = ?`, descr, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupDeployMeta records a group's deployment provenance
// (JOH-245): the mission it was deployed against and the template it was
// instantiated/deployed from. Either may be "" (no mission / no source
// template). Returns the number of rows affected — 0 means no group by
// that name. Called best-effort right after CreateAgentGroup on the
// deploy / instantiate path, mirroring SetAgentGroupDefaultCwd.
func SetAgentGroupDeployMeta(name, mission, sourceTemplate string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	templateID, err := registryIDByNameDB(db, "group_templates", sourceTemplate)
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups SET mission = ?, source_template = ?, source_template_id = ? WHERE name = ?`,
		mission, sourceTemplate, templateID, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupDefaultCwd sets (or, with cwd == "", clears) the
// default working directory pre-filled for agents spawned into the
// named group. Returns the number of rows affected — 0 means no
// group by that name, so the caller can answer 404.
func SetAgentGroupDefaultCwd(name, cwd string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups SET default_cwd = ? WHERE name = ?`, cwd, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupDefaultContext sets (or, with context == "", clears)
// the shared startup context for the named group. Returns the number
// of rows affected — 0 means no group by that name, so the caller can
// answer 404.
func SetAgentGroupDefaultContext(name, context string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups SET default_context = ? WHERE name = ?`, context, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupDefaultProfile sets (or, with profile == "", clears) the
// name of the spawn profile whose launch fields fill blank spawn fields
// for agents spawned into the named group (JOH-210). Callers that stamp a
// name for immediate use validate the referenced profile exists first;
// callers that resolve the profile LIVE at spawn time (the scribe summon,
// JOH-371) may deliberately stamp a name whose profile can later vanish —
// a dangling reference self-heals to the no-profile path at read time
// (groupDefaultProfile → nil) rather than erroring here. Returns the
// number of rows affected — 0 means no group by that name, so the caller
// can answer 404.
func SetAgentGroupDefaultProfile(name, profile string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	profileID, err := registryIDByNameDB(db, "spawn_profiles", profile)
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups SET default_profile = ?, default_profile_id = ? WHERE name = ?`, profile, profileID, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupMaxMembers sets the hard member-count cap for the named
// group. 0 means unlimited (the default). A spawn that would push the
// group's membership over a non-zero cap is refused — see the
// spawn-guardrail layer. Returns the number of rows affected — 0 means
// no group by that name, so the caller can answer 404. A negative max
// is clamped to 0 (unlimited) rather than rejected, so a careless CLI
// value never wedges a group.
func SetAgentGroupMaxMembers(name string, max int) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	if max < 0 {
		max = 0
	}
	res, err := db.Exec(`UPDATE agent_groups SET max_members = ? WHERE name = ?`, max, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupNotifyEnabled flips the group's OS-notification switch.
// false mutes state-transition notifications for every member agent
// (a per-agent 'on' pref in agent_notify_prefs still overrides the
// mute). Returns the number of rows affected — 0 means no group by
// that name, so the caller can answer 404.
func SetAgentGroupNotifyEnabled(name string, enabled bool) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups SET notify_enabled = ? WHERE name = ?`, enabled, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentGroupRemoteControl sets the group's remote-control policy — the
// tri-state knob that OVERRIDES a spawn profile's remote-control default
// (JOH-262). policy is nil = inherit (clear the override, defer to the profile),
// false = actively deny (force Remote Access off for the group's agents), true =
// actively opt-in (force it on). Stored as a nullable INTEGER so "inherit" is a
// real state distinct from "off". Returns the number of rows affected — 0 means
// no group by that name, so the caller can answer 404.
func SetAgentGroupRemoteControl(name string, policy *bool) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups SET remote_control = ? WHERE name = ?`,
		boolPtrToNull(policy), name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteAgentGroup removes a group by name. Cascades to membership +
// ownership rows (ON DELETE CASCADE in schema) and, within the same
// transaction, rewrites the group's messages to group_id = 0 so the
// conversation history survives the group's deletion as direct
// messages. agent_messages no longer has a foreign key to
// agent_groups (the "universal inbox" change dropped it), so this is a
// data-retention choice rather than an FK workaround: a deleted group
// should not silently destroy what its members said to each other.
//
// No-op if the group doesn't exist.
func DeleteAgentGroup(name string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var gID int64
	if err := tx.QueryRow(`SELECT id FROM agent_groups WHERE name = ?`, name).Scan(&gID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if _, err := tx.Exec(`UPDATE agent_messages SET group_id = 0 WHERE group_id = ?`, gID); err != nil {
		return err
	}
	// Advisory process state (JOH-242) is keyed to the group by group_id in
	// sibling tables (unlike deploy meta, which lives on the agent_groups row
	// and dies with it), so sweep it explicitly in the same transaction.
	if _, err := tx.Exec(`DELETE FROM group_process_state WHERE group_id = ?`, gID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM group_process_transitions WHERE group_id = ?`, gID); err != nil {
		return err
	}
	// Staged-spawn choreography state (JOH-244) is keyed to the group by
	// group_id, so cancel any pending waves in the same transaction (the v92
	// process-state cleanup pattern). A wave runner that reads a now-missing
	// row simply drops it — self-healing.
	if _, err := tx.Exec(`DELETE FROM group_wave_choreography WHERE group_id = ?`, gID); err != nil {
		return err
	}
	// Group-target cron jobs (JOH-244) — including template-seeded rhythms —
	// target THIS group and become meaningless once it is gone, so remove them
	// here (agent_cron_runs cascade-clean via their job FK). Conv-target jobs
	// merely routed THROUGH the group still deliver to their conv and are left
	// alone. A fire against an already-deleted group also no-ops gracefully
	// (fireCronGroupJob resolves the group to nil → "no_target"), so this is
	// tidy-up, not correctness-critical.
	if _, err := tx.Exec(
		`DELETE FROM agent_cron_jobs WHERE target_kind = 'group' AND group_id = ?`, gID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM agent_groups WHERE id = ?`, gID); err != nil {
		return err
	}
	return tx.Commit()
}

const agentGroupSelect = `SELECT id, name, descr, default_cwd, default_context,
	CASE WHEN default_profile_id IS NULL THEN default_profile
	     ELSE COALESCE((SELECT name FROM spawn_profiles WHERE id = default_profile_id), '') END,
	CASE WHEN sandbox_profile_id IS NULL THEN sandbox_profile
	     ELSE COALESCE((SELECT name FROM sandbox_profiles WHERE id = sandbox_profile_id), '') END,
	COALESCE(sandbox_profile_id, 0),
	max_members, notify_enabled, remote_control, mission,
	CASE WHEN source_template_id IS NULL THEN source_template
	     ELSE COALESCE((SELECT name FROM group_templates WHERE id = source_template_id), source_template) END,
	COALESCE(source_template_id, 0),
	created_at, archived_at, parent_id FROM agent_groups`

const agentGroupAliasedSelect = `SELECT g.id, g.name, g.descr, g.default_cwd, g.default_context,
	CASE WHEN g.default_profile_id IS NULL THEN g.default_profile
	     ELSE COALESCE((SELECT name FROM spawn_profiles WHERE id = g.default_profile_id), '') END,
	CASE WHEN g.sandbox_profile_id IS NULL THEN g.sandbox_profile
	     ELSE COALESCE((SELECT name FROM sandbox_profiles WHERE id = g.sandbox_profile_id), '') END,
	COALESCE(g.sandbox_profile_id, 0),
	g.max_members, g.notify_enabled, g.remote_control, g.mission,
	CASE WHEN g.source_template_id IS NULL THEN g.source_template
	     ELSE COALESCE((SELECT name FROM group_templates WHERE id = g.source_template_id), g.source_template) END,
	COALESCE(g.source_template_id, 0),
	g.created_at, g.archived_at, g.parent_id`

// GetAgentGroupByName returns the group with the given name, or nil if not
// found.
func GetAgentGroupByName(name string) (*AgentGroup, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(agentGroupSelect+` WHERE name = ?`, name)
	g, err := scanAgentGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return g, err
}

// GetAgentGroupByID returns the group with the given primary key, or
// nil if not found.
func GetAgentGroupByID(id int64) (*AgentGroup, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(agentGroupSelect+` WHERE id = ?`, id)
	g, err := scanAgentGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return g, err
}

// ListAgentGroups returns all groups, ordered by name.
func ListAgentGroups() ([]*AgentGroup, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(agentGroupSelect + ` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*AgentGroup
	for rows.Next() {
		g, err := scanAgentGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ErrGroupNameTaken signals that a rename target already exists.
var ErrGroupNameTaken = errors.New("group name already exists")

// RenameAgentGroup atomically renames the canonical name of a group +
// records an audit row. byConv may be empty for the human path.
//
// Returns:
//   - (nil, nil) when no group with oldName exists — caller chooses
//     whether to surface that as 404 or no-op.
//   - (nil, ErrGroupNameTaken) if newName collides with another group.
//   - (group, nil) on success — the returned group reflects the NEW
//     name + the same id/created_at/archived_at.
//
// All work happens in a single transaction so a failure mid-flight
// rolls back cleanly. The schema stores group references as integer
// foreign keys (group_id), so renaming is a single-row UPDATE — no
// cascades required.
func RenameAgentGroup(oldName, newName, byConv string) (*AgentGroup, error) {
	if newName == "" {
		return nil, fmt.Errorf("newName required")
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRow(
		agentGroupSelect+` WHERE name = ?`,
		oldName)
	g, err := scanAgentGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if oldName == newName {
		// No-op: idempotent same-name. Still record the audit row so a
		// human can see they tried — useful when chasing typos.
		if _, err := tx.Exec(
			`INSERT INTO agent_group_audit (group_id, old_name, new_name, by_conv, at, by_agent)
			 VALUES (?, ?, ?, ?, ?, `+agentForConvExpr+`)`,
			g.ID, oldName, newName, byConv, time.Now().Format(time.RFC3339Nano), byConv); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return g, nil
	}
	// Collision check before the UPDATE to surface a clear 409, since
	// the UNIQUE constraint would fire as a generic SQLITE_CONSTRAINT.
	var existing int64
	err = tx.QueryRow(`SELECT id FROM agent_groups WHERE name = ?`, newName).Scan(&existing)
	if err == nil {
		return nil, ErrGroupNameTaken
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE agent_groups SET name = ? WHERE id = ?`, newName, g.ID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		`INSERT INTO agent_group_audit (group_id, old_name, new_name, by_conv, at, by_agent)
		 VALUES (?, ?, ?, ?, ?, `+agentForConvExpr+`)`,
		g.ID, oldName, newName, byConv, time.Now().Format(time.RFC3339Nano), byConv); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	g.Name = newName
	return g, nil
}

// ListAgentGroupRenames returns the rename history for a single group,
// most recent first. Used for "what was this group called before?"
// lookups; not currently surfaced via CLI but cheap to expose later.
func ListAgentGroupRenames(groupID int64) ([]AgentGroupAudit, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(
		`SELECT id, group_id, old_name, new_name, by_conv, at, by_agent
		 FROM agent_group_audit WHERE group_id = ? ORDER BY id DESC`, groupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AgentGroupAudit
	for rows.Next() {
		var a AgentGroupAudit
		if err := rows.Scan(&a.ID, &a.GroupID, &a.OldName, &a.NewName, &a.ByConv, &a.At, &a.ByAgent); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AgentGroupAudit is one row in agent_group_audit, recording a single
// rename event. ByConv is the conv-id snapshot of who renamed the group;
// ByAgent is the stable agent_id companion (dual-written via
// agentForConvExpr, v77-backfilled) — the durable actor, with ByConv kept
// as the point-in-time snapshot. ByAgent is "" for a human/un-enrolled
// renamer.
type AgentGroupAudit struct {
	ID      int64
	GroupID int64
	OldName string
	NewName string
	ByConv  string
	At      string
	ByAgent string
}

// ArchiveAgentGroup soft-deletes a group: stamps archived_at = now and
// leaves all rows intact. Mutating operations (member.add, owners.*,
// message) refuse on archived groups; listing endpoints filter them out
// by default. Idempotent — re-archiving an already-archived group bumps
// the timestamp without erroring. Returns sql.ErrNoRows if the group
// doesn't exist (callers should treat as a clean miss).
func ArchiveAgentGroup(name string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE agent_groups SET archived_at = ? WHERE name = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UnarchiveAgentGroup reverses ArchiveAgentGroup: clears archived_at so
// the group is active again. Idempotent on already-active groups.
func UnarchiveAgentGroup(name string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE agent_groups SET archived_at = '' WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ErrGroupParentNotFound is returned by SetAgentGroupParent when the named
// parent group does not exist.
var ErrGroupParentNotFound = errors.New("parent group not found")

// ErrGroupParentCycle is returned by SetAgentGroupParent when the requested
// nesting would create a loop — a group cannot be its own parent, nor be
// nested under a group that is already one of its own descendants.
var ErrGroupParentCycle = errors.New("cannot nest a group under itself or one of its own descendants")

// SetAgentGroupParent nests the group childID under the group named
// parentName (n-level groups-in-groups, JOH-392). parentName == "" (after
// trimming) clears the parent, making the child top-level. Returns the
// updated child group.
//
// Cycle safety: rejects self-parenting and any edge that would close a loop.
// The check walks UP the prospective parent's ancestor chain; sighting the
// child means the child is already an ancestor of the parent, so the new
// edge would loop — refuse with ErrGroupParentCycle. A visited set bounds
// the walk so even a pre-existing corrupt cycle in the table cannot spin
// forever. Returns ErrGroupParentNotFound if parentName resolves to nothing,
// and sql.ErrNoRows if the child vanished mid-flight.
//
// All work is one transaction; the child row's parent_id is the only write.
// (Deleting a parent needs no code here — the column's ON DELETE SET NULL
// FK auto-orphans children back to top-level; see migrateV98toV99.)
func SetAgentGroupParent(childID int64, parentName string) (*AgentGroup, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var newParent sql.NullInt64
	if parentName = strings.TrimSpace(parentName); parentName != "" {
		var pid int64
		err := tx.QueryRow(`SELECT id FROM agent_groups WHERE name = ?`, parentName).Scan(&pid)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrGroupParentNotFound
		} else if err != nil {
			return nil, err
		}
		if pid == childID {
			return nil, ErrGroupParentCycle
		}
		// Walk up from the prospective parent. If we reach childID, the child
		// is already an ancestor of pid and this edge would loop.
		seen := map[int64]bool{}
		cur := sql.NullInt64{Int64: pid, Valid: true}
		for cur.Valid {
			if cur.Int64 == childID {
				return nil, ErrGroupParentCycle
			}
			if seen[cur.Int64] {
				break // pre-existing corrupt cycle in the table; stop walking
			}
			seen[cur.Int64] = true
			var next sql.NullInt64
			err := tx.QueryRow(`SELECT parent_id FROM agent_groups WHERE id = ?`, cur.Int64).Scan(&next)
			if errors.Is(err, sql.ErrNoRows) {
				break
			} else if err != nil {
				return nil, err
			}
			cur = next
		}
		newParent = sql.NullInt64{Int64: pid, Valid: true}
	}

	res, err := tx.Exec(`UPDATE agent_groups SET parent_id = ? WHERE id = ?`, newParent, childID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return GetAgentGroupByID(childID)
}

// AddAgentGroupMember inserts (or replaces) a member in a group.
func AddAgentGroupMember(m *AgentGroupMember) error {
	db, err := Open()
	if err != nil {
		return err
	}
	if m.JoinedAt.IsZero() {
		m.JoinedAt = time.Now()
	}
	// Joining a group makes the conv an agent. EnsureAgentForConv mints / links
	// the stable actor; the membership is then keyed on agent_id (JOH-26) so it
	// survives conv rotations without a rekey. Insert-only — a stray add never
	// un-retires; the dashboard add-member flow reinstates retired targets
	// explicitly.
	if _, _, err := EnsureAgentForConv(m.ConvID, "group"); err != nil {
		return err
	}
	agentID, err := AgentIDForConv(m.ConvID)
	if err != nil {
		return err
	}
	if agentID == "" {
		return fmt.Errorf("AddAgentGroupMember: no actor for conv %s", m.ConvID)
	}
	_, err = db.Exec(`INSERT OR REPLACE INTO agent_group_members
		(group_id, agent_id, role, descr, joined_at)
		VALUES (?, ?, ?, ?, ?)`,
		m.GroupID, agentID, m.Role, m.Descr,
		m.JoinedAt.Format(time.RFC3339Nano))
	return err
}

// UpdateAgentGroupMember patches non-nil fields on an existing member.
// Pass a nil pointer for any field you don't want to change. Returns
// (rowsAffected, error); 0 rows means no such (group, conv) pair.
func UpdateAgentGroupMember(groupID int64, convID string, role, descr *string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	sets := []string{}
	args := []any{}
	if role != nil {
		sets = append(sets, "role = ?")
		args = append(args, *role)
	}
	if descr != nil {
		sets = append(sets, "descr = ?")
		args = append(args, *descr)
	}
	if len(sets) == 0 {
		return 0, nil
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return 0, err
	}
	if agentID == "" {
		return 0, nil
	}
	args = append(args, groupID, agentID)
	q := "UPDATE agent_group_members SET " + sets[0]
	for i := 1; i < len(sets); i++ {
		q += ", " + sets[i]
	}
	q += " WHERE group_id = ? AND agent_id = ?"
	res, err := db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RemoveAgentGroupMember removes a (group, conv) pair. Idempotent.
func RemoveAgentGroupMember(groupID int64, convID string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return err
	}
	if agentID == "" {
		return nil
	}
	_, err = db.Exec(`DELETE FROM agent_group_members WHERE group_id = ? AND agent_id = ?`,
		groupID, agentID)
	return err
}

// AgentDeletionCounts records what `DeleteAgentByConvID` removed,
// per affected table. Surfaced in the daemon response so the human
// (or the audit log) can confirm scope.
type AgentDeletionCounts struct {
	GroupMembers   int64 `json:"group_members"`
	GroupOwners    int64 `json:"group_owners"`
	MessagesFrom   int64 `json:"messages_from"`
	MessagesTo     int64 `json:"messages_to"`
	Permissions    int64 `json:"permissions"`
	CronJobsOwned  int64 `json:"cron_jobs_owned"`
	CronJobsTarget int64 `json:"cron_jobs_target"`
	SuccessionOld  int64 `json:"succession_old"`
	SuccessionNew  int64 `json:"succession_new"`
	Embeddings     int64 `json:"embeddings"`
	ConvIndex      int64 `json:"conv_index"`
	Sessions       int64 `json:"sessions"`
	NotifyPrefs    int64 `json:"notify_prefs"`
	SudoGrants     int64 `json:"sudo_grants"`
	SpawnHistory   int64 `json:"spawn_history"`
	CloneHistory   int64 `json:"clone_history"`
}

// Add accumulates o into c field-by-field. Used by the actor-level
// cross-generation delete (conv.DeleteAgentAllGenerations, JOH-26 PR3d) to sum
// each swept generation's per-table removals into one reported total.
func (c *AgentDeletionCounts) Add(o AgentDeletionCounts) {
	c.GroupMembers += o.GroupMembers
	c.GroupOwners += o.GroupOwners
	c.MessagesFrom += o.MessagesFrom
	c.MessagesTo += o.MessagesTo
	c.Permissions += o.Permissions
	c.CronJobsOwned += o.CronJobsOwned
	c.CronJobsTarget += o.CronJobsTarget
	c.SuccessionOld += o.SuccessionOld
	c.SuccessionNew += o.SuccessionNew
	c.Embeddings += o.Embeddings
	c.ConvIndex += o.ConvIndex
	c.Sessions += o.Sessions
	c.NotifyPrefs += o.NotifyPrefs
	c.SudoGrants += o.SudoGrants
	c.SpawnHistory += o.SpawnHistory
	c.CloneHistory += o.CloneHistory
}

// DeleteAgentByConvID purges the conversation generation convID, plus —
// when convID is its actor's live generation — the actor's identity. Single
// transaction; partial failure rolls everything back.
//
// Conv-scoped (always, keyed on convID — these belong to THIS generation):
//
//   - agent_messages (from_conv = ? OR to_conv = ?)
//   - agent_conv_succession (old_conv_id = ? OR new_conv_id = ?)
//   - conv_embeddings
//   - conv_index
//   - sessions
//
// Actor-scoped (keyed on the resolved agent_id) — ONLY when convID is the
// actor's current_conv_id (its live generation):
//
//   - agent_group_members, agent_group_owners, agent_permissions,
//     agent_notify_prefs, agent_sudo_grants
//   - agent_cron_jobs (owner_agent = ? OR target_agent = ?), agent-keyed since
//     JOH-26 PR3a. Each delete cascades to agent_cron_runs via the FK.
//   - agent_spawn_history (spawner_agent_id), agent_clone_history
//     (source_agent_id) — agent-keyed rate-limit history with no FK to agents,
//     so deleted explicitly here rather than via cascade.
//   - agents (cascades the remaining agent_conversations links)
//
// Deleting a PREDECESSOR generation (a reincarnate / Claude Code /clear keeps
// the old conv around) instead only unlinks that one conv from its actor — the
// live actor and its identity survive (JOH-26). The succession chain is healed
// (a middle-generation delete re-links pred→succ) and any head_alias anchored on
// the deleted predecessor is rebased onto its successor (JOH-330), so stale-id
// forwarding and alias resolution both still reach the live head. When convID is
// the live generation and the actor has older generations, those predecessors'
// own conv-scoped rows + .jsonl are left behind (cleaning them up across an
// actor's whole generation set is a later stage).
//
// Filesystem state (the .jsonl in ~/.claude/projects/... and the
// ~/.claude/session-env/<convID> file) is the caller's
// responsibility — DeleteAgentByConvID only touches SQLite. The
// daemon handler combines this with file-system cleanup.
//
// Idempotent: calling on a conv-id that doesn't exist returns
// AgentDeletionCounts{} with no error.
func DeleteAgentByConvID(convID string) (AgentDeletionCounts, error) {
	var c AgentDeletionCounts
	if convID == "" {
		return c, fmt.Errorf("convID is required")
	}
	d, err := Open()
	if err != nil {
		return c, err
	}
	tx, err := d.Begin()
	if err != nil {
		return c, err
	}
	defer func() { _ = tx.Rollback() }()

	agentID, err := agentIDForConvTx(tx, convID)
	if err != nil {
		return c, err
	}

	// Capture this conv's succession neighbours BEFORE the conv-scoped loop
	// below wipes its edges. If convID turns out to be a MIDDLE predecessor
	// generation (it has BOTH a predecessor and a successor), deleting it
	// removes pred→convID and convID→succ, which would strand any OLDER
	// generation's stale-id forwarding (ResolveLatestConv walks
	// agent_conv_succession, not agent_conversations). The predecessor branch
	// below re-links pred→succ so an ancestor still resolves forward to the
	// live head. bridgeReason carries the incoming edge's reason (how the
	// predecessor was superseded). Both empty for a head / genesis / non-agent.
	var bridgeOld, bridgeNew, bridgeReason string
	if err := tx.QueryRow(`SELECT old_conv_id, reason FROM agent_conv_succession
		WHERE new_conv_id = ? ORDER BY succeeded_at DESC, rowid DESC LIMIT 1`, convID).
		Scan(&bridgeOld, &bridgeReason); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return c, err
	}
	if err := tx.QueryRow(`SELECT new_conv_id FROM agent_conv_succession
		WHERE old_conv_id = ?`, convID).Scan(&bridgeNew); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return c, err
	}

	// Conv-scoped teardown: this conversation generation's runtime, history,
	// index and message rows. Always removed — they belong to THIS generation,
	// not to the actor as a whole.
	type convStep struct {
		stmt string
		into *int64
	}
	for _, s := range []convStep{
		{`DELETE FROM agent_messages WHERE from_conv = ?`, &c.MessagesFrom},
		{`DELETE FROM agent_messages WHERE to_conv = ?`, &c.MessagesTo},
		{`DELETE FROM agent_conv_succession WHERE old_conv_id = ?`, &c.SuccessionOld},
		{`DELETE FROM agent_conv_succession WHERE new_conv_id = ?`, &c.SuccessionNew},
		{`DELETE FROM conv_embeddings WHERE conv_id = ?`, &c.Embeddings},
		{`DELETE FROM conv_index WHERE conv_id = ?`, &c.ConvIndex},
		{`DELETE FROM sessions WHERE conv_id = ?`, &c.Sessions},
	} {
		res, err := tx.Exec(s.stmt, convID)
		if err != nil {
			return AgentDeletionCounts{}, fmt.Errorf("delete agent (%s): %w", s.stmt, err)
		}
		n, _ := res.RowsAffected()
		*s.into = n
	}

	// Actor-scoped teardown: the agent_id-keyed identity rows (memberships,
	// ownerships, permission overrides, notify pref) and the actor row itself.
	// Only when convID is the actor's CURRENT (live) generation. Under JOH-26 a
	// reincarnate / Claude Code /clear leaves the old conv around as a past
	// GENERATION of the same active actor — deleting one of those predecessors
	// must NOT wipe the live actor's identity. So:
	//   - current generation  → tear the whole actor down; the `agents` delete
	//     cascades every remaining agent_conversations link.
	//   - predecessor generation → unlink just this conv; the actor and its
	//     identity (and other generations) stay put.
	// agentID == "" means the conv is not an agent at all — nothing actor-level.
	if agentID != "" {
		var current string
		switch err := tx.QueryRow(
			`SELECT current_conv_id FROM agents WHERE agent_id = ?`, agentID).Scan(&current); {
		case errors.Is(err, sql.ErrNoRows):
			// Actor row already gone (e.g. an earlier partial delete) — clear any
			// stale generation link this conv still holds.
			if _, err := tx.Exec(`DELETE FROM agent_conversations WHERE conv_id = ?`, convID); err != nil {
				return AgentDeletionCounts{}, fmt.Errorf("delete agent (unlink generation): %w", err)
			}
		case err != nil:
			return c, err
		case current == convID:
			type actorStep struct {
				stmt string
				into *int64
			}
			for _, s := range []actorStep{
				{`DELETE FROM agent_group_members WHERE agent_id = ?`, &c.GroupMembers},
				{`DELETE FROM agent_group_owners WHERE agent_id = ?`, &c.GroupOwners},
				{`DELETE FROM agent_permissions WHERE agent_id = ?`, &c.Permissions},
				{`DELETE FROM agent_notify_prefs WHERE agent_id = ?`, &c.NotifyPrefs},
				// Cron jobs are agent-keyed (JOH-26 PR3a): a job belongs to its
				// owner/target actor, so it is torn down with the actor (and only
				// then) — deleting a predecessor generation leaves the live
				// actor's schedules intact. Each delete cascades to
				// agent_cron_runs via the FK.
				{`DELETE FROM agent_cron_jobs WHERE owner_agent = ?`, &c.CronJobsOwned},
				{`DELETE FROM agent_cron_jobs WHERE target_agent = ?`, &c.CronJobsTarget},
				// Sudo grants and the agent-keyed spawn/clone rate-limit history
				// (JOH-26 PR2/PR3a) are keyed on agent_id but have NO FK to
				// agents, so the `agents` delete below does not cascade them.
				// Tear them down explicitly with the actor — otherwise they
				// orphan (the group export / count paths JOIN through agents, so
				// orphaned rows become invisible residue). Actor-scoped: a
				// predecessor delete leaves the live actor's grants + history
				// intact. This delete-set mirrors the identity-bearing set
				// absorbBareSuccessorActorTx guards on.
				{`DELETE FROM agent_sudo_grants WHERE agent_id = ?`, &c.SudoGrants},
				{`DELETE FROM agent_spawn_history WHERE spawner_agent_id = ?`, &c.SpawnHistory},
				{`DELETE FROM agent_clone_history WHERE source_agent_id = ?`, &c.CloneHistory},
				{`DELETE FROM agents WHERE agent_id = ?`, nil}, // cascades agent_conversations
			} {
				res, err := tx.Exec(s.stmt, agentID)
				if err != nil {
					return AgentDeletionCounts{}, fmt.Errorf("delete agent (%s): %w", s.stmt, err)
				}
				if s.into != nil {
					n, _ := res.RowsAffected()
					*s.into = n
				}
			}
		default:
			// Predecessor generation: unlink just this conv; the actor lives on.
			if _, err := tx.Exec(`DELETE FROM agent_conversations WHERE conv_id = ?`, convID); err != nil {
				return AgentDeletionCounts{}, fmt.Errorf("delete agent (unlink generation): %w", err)
			}
			// Heal the succession chain when this was a MIDDLE generation: the
			// conv-scoped loop above removed its incoming (pred→convID) and
			// outgoing (convID→succ) edges, so re-link pred→succ. Without this a
			// stale reference to an ANCESTOR the caller did NOT delete would stop
			// forwarding to the live head. Idempotent upsert on old_conv_id (the
			// chain holds one successor per old conv). Skipped for a genesis
			// (no predecessor) or a pre-head (no successor) generation.
			if bridgeOld != "" && bridgeNew != "" && bridgeOld != bridgeNew {
				if _, err := tx.Exec(`INSERT INTO agent_conv_succession
					(old_conv_id, new_conv_id, reason, succeeded_at, agent_id)
					VALUES (?, ?, ?, ?, `+agentForSuccessionExpr+`)
					ON CONFLICT(old_conv_id) DO UPDATE SET
						new_conv_id = excluded.new_conv_id,
						reason = excluded.reason,
						succeeded_at = excluded.succeeded_at,
						agent_id = excluded.agent_id`,
					bridgeOld, bridgeNew, bridgeReason,
					time.Now().UTC().Format(time.RFC3339), bridgeNew, bridgeOld); err != nil {
					return AgentDeletionCounts{}, fmt.Errorf("delete agent (bridge succession): %w", err)
				}
			}
			// Rebase any head_alias conv-anchored on THIS deleted predecessor onto
			// its immediate successor — the head_alias twin of the bridge above
			// (JOH-330). The conv-scoped loop wiped convID's anchor→successor edge,
			// so an alias left on convID would resolve (ResolveHeadAlias →
			// ResolveLatestConv) to the now-dead anchor instead of the live head;
			// forwarding the anchor one hop keeps it on the live succession chain, so
			// the alias's own chain-walk still lands on the head. bridgeNew is the
			// same forward target the succession bridge uses and is always present
			// for a non-head generation (the rotation off convID recorded the edge);
			// a no-op when no alias is anchored here. The live-head delete falls in
			// the actor-teardown branch above, where the alias legitimately breaks.
			if bridgeNew != "" && bridgeNew != convID {
				if _, err := rebaseHeadAliasAnchorTx(tx, convID, bridgeNew); err != nil {
					return AgentDeletionCounts{}, fmt.Errorf("delete agent (rebase head alias): %w", err)
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return AgentDeletionCounts{}, err
	}
	return c, nil
}

// RemoveAllAgentGroupMembershipsForConv drops every membership row
// referencing convID, regardless of group. Used by the conv-delete
// path so the dashboard's agent listing doesn't keep showing
// orphaned (unknown) entries after the underlying conversation is
// wiped. Idempotent — returns the number of rows removed.
func RemoveAllAgentGroupMembershipsForConv(convID string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return 0, err
	}
	if agentID == "" {
		return 0, nil
	}
	res, err := db.Exec(`DELETE FROM agent_group_members WHERE agent_id = ?`, agentID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListAgentGroupMembers returns the members of a group, ordered by joined_at
// then conv_id. The membership is agent-keyed (JOH-26); the ConvID field is
// resolved to each actor's CURRENT conv for display/delivery.
func ListAgentGroupMembers(groupID int64) ([]*AgentGroupMember, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT m.group_id, ag.current_conv_id, m.role, m.descr, m.joined_at
		FROM agent_group_members m JOIN agents ag ON ag.agent_id = m.agent_id
		WHERE m.group_id = ? ORDER BY m.joined_at, ag.current_conv_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*AgentGroupMember
	for rows.Next() {
		m, err := scanAgentGroupMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListGroupsForConv returns all groups that include the given conv_id,
// ordered by group name. conv_id is only the resolution input — it
// resolves to the stable actor and delegates to ListGroupsForAgent, so
// membership is read by agent and survives conv rotations.
func ListGroupsForConv(convID string) ([]*AgentGroup, error) {
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return nil, err
	}
	return ListGroupsForAgent(agentID)
}

// ListGroupsForAgent returns all groups that include the given stable
// agent_id, ordered by group name. This is the agent-keyed primitive
// behind ListGroupsForConv; the empty/unknown agent has no groups.
func ListGroupsForAgent(agentID string) ([]*AgentGroup, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, nil
	}
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(agentGroupAliasedSelect+`
		FROM agent_groups g
		JOIN agent_group_members m ON m.group_id = g.id
		WHERE m.agent_id = ?
		ORDER BY g.name`, agentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*AgentGroup
	for rows.Next() {
		g, err := scanAgentGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GroupNamesByConv returns a map from conv-id to its group names, alphabetised.
// Convs not in any group are absent from the map. The query scans the whole
// membership table (which is small — humans curate it manually), so callers
// don't need to pass a conv list.
func GroupNamesByConv() (map[string][]string, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	// Membership is agent-keyed; resolve to each actor's current conv for the
	// display-facing conv→group-names map.
	rows, err := db.Query(`SELECT ag.current_conv_id, g.name
		FROM agent_group_members m
		JOIN agents ag ON ag.agent_id = m.agent_id
		JOIN agent_groups g ON g.id = m.group_id
		ORDER BY ag.current_conv_id, g.name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string][]string{}
	for rows.Next() {
		var conv, name string
		if err := rows.Scan(&conv, &name); err != nil {
			return nil, err
		}
		out[conv] = append(out[conv], name)
	}
	return out, rows.Err()
}

// SharedGroupsForConvs returns the groups containing both convs, ordered by
// group name. Used by `agent message` to authorise sender→target.
func SharedGroupsForConvs(a, b string) ([]*AgentGroup, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	agentA, err := AgentIDForConv(a)
	if err != nil {
		return nil, err
	}
	agentB, err := AgentIDForConv(b)
	if err != nil {
		return nil, err
	}
	if agentA == "" || agentB == "" {
		return nil, nil
	}
	rows, err := db.Query(agentGroupAliasedSelect+`
		FROM agent_groups g
		JOIN agent_group_members ma ON ma.group_id = g.id AND ma.agent_id = ?
		JOIN agent_group_members mb ON mb.group_id = g.id AND mb.agent_id = ?
		ORDER BY g.name`, agentA, agentB)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*AgentGroup
	for rows.Next() {
		g, err := scanAgentGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// FindMemberInGroup returns the member entry for (group, conv) or nil if not
// present.
func FindMemberInGroup(groupID int64, convID string) (*AgentGroupMember, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return nil, err
	}
	if agentID == "" {
		return nil, nil
	}
	row := db.QueryRow(`SELECT m.group_id, ag.current_conv_id, m.role, m.descr, m.joined_at
		FROM agent_group_members m JOIN agents ag ON ag.agent_id = m.agent_id
		WHERE m.group_id = ? AND m.agent_id = ?`, groupID, agentID)
	m, err := scanAgentGroupMember(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
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
	res, err := db.Exec(`INSERT INTO agent_messages
		(group_id, from_conv, to_conv, from_agent, to_agent, subject, body, parent_id,
		 created_at, delivered_at, read_at,
		 to_recipients, cc_recipients, to_recipient_agents, cc_recipient_agents, original_to_conv, pin_gen)
		VALUES (?, ?, ?,
		 COALESCE((SELECT agent_id FROM agent_conversations WHERE conv_id = ?), ''),
		 COALESCE((SELECT agent_id FROM agent_conversations WHERE conv_id = ?), ''),
		 ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.GroupID, m.FromConv, m.ToConv, m.FromConv, m.ToConv, m.Subject, m.Body, m.ParentID,
		m.CreatedAt.Format(time.RFC3339Nano),
		formatTimeOrEmpty(m.DeliveredAt), formatTimeOrEmpty(m.ReadAt),
		recipientsToJSON(m.ToRecipients), recipientsToJSON(m.CcRecipients),
		recipientAgentsJSON(db, m.ToRecipients), recipientAgentsJSON(db, m.CcRecipients),
		m.OriginalToConv, pinGen)
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
	cutoff := olderThan.Format(time.RFC3339Nano)
	q := `DELETE FROM agent_messages
		WHERE (from_conv = ? OR to_conv = ?)
		  AND created_at < ?`
	args := []any{forConv, forConv, cutoff}
	if readOnly {
		q += ` AND read_at != ''`
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
	args = append(args, olderThan.Format(time.RFC3339Nano))
	if readOnly {
		q += ` AND read_at != ''`
	}
	res, err := d.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// AgentGroupOwner is a row in agent_group_owners. Owners can message
// a group's members and multicast to the group without being members
// themselves. Distinct from membership so the "X is an owner but
// not a peer" case is representable.
type AgentGroupOwner struct {
	GroupID int64
	// AgentID is the owner's stable actor key — ownership is keyed on it
	// (JOH-26), so it is the canonical, rotation-immune identity to display.
	AgentID   string
	ConvID    string
	GrantedAt time.Time
	GrantedBy string
}

// AddAgentGroupOwner records convID as an owner of groupID. Idempotent
// (INSERT OR IGNORE). grantedBy is "" for human-issued grants, a
// conv-id when an agent with permissions.grant did it.
func AddAgentGroupOwner(groupID int64, convID, grantedBy string) error {
	if convID == "" {
		return fmt.Errorf("conv_id required")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	// Owning a group makes the conv an agent. EnsureAgentForConv mints / links
	// the actor; ownership is then keyed on agent_id (JOH-26).
	if _, _, err := EnsureAgentForConv(convID, "group"); err != nil {
		return err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return err
	}
	if agentID == "" {
		return fmt.Errorf("AddAgentGroupOwner: no actor for conv %s", convID)
	}
	_, err = d.Exec(
		`INSERT OR IGNORE INTO agent_group_owners (group_id, agent_id, granted_at, granted_by)
		 VALUES (?, ?, ?, ?)`,
		groupID, agentID, time.Now().Format(time.RFC3339Nano), grantedBy)
	return err
}

// RemoveAgentGroupOwner clears an ownership row. Returns the number
// of rows removed (0 when convID wasn't an owner).
func RemoveAgentGroupOwner(groupID int64, convID string) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return 0, err
	}
	if agentID == "" {
		return 0, nil
	}
	res, err := d.Exec(
		`DELETE FROM agent_group_owners WHERE group_id = ? AND agent_id = ?`,
		groupID, agentID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RemoveAllAgentGroupOwnershipsForConv drops every ownership row
// referencing convID, regardless of group. Bulk cleanup variant for
// the conv-delete path. Idempotent — returns the number of rows
// removed.
func RemoveAllAgentGroupOwnershipsForConv(convID string) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return 0, err
	}
	if agentID == "" {
		return 0, nil
	}
	res, err := d.Exec(`DELETE FROM agent_group_owners WHERE agent_id = ?`, agentID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// IsAgentGroupOwner returns true when convID's actor owns groupID. Resolves
// the conv to its stable agent_id (JOH-26), so ANY generation of the actor —
// not just its current conv — answers correctly.
func IsAgentGroupOwner(groupID int64, convID string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return false, err
	}
	if agentID == "" {
		return false, nil
	}
	var n int
	err = d.QueryRow(
		`SELECT COUNT(*) FROM agent_group_owners WHERE group_id = ? AND agent_id = ?`,
		groupID, agentID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListAgentGroupOwners returns every owner row for the given group,
// most-recently-granted first.
func ListAgentGroupOwners(groupID int64) ([]*AgentGroupOwner, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(
		`SELECT o.group_id, o.agent_id, ag.current_conv_id, o.granted_at, o.granted_by
		 FROM agent_group_owners o JOIN agents ag ON ag.agent_id = o.agent_id
		 WHERE o.group_id = ?
		 ORDER BY o.granted_at DESC`,
		groupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*AgentGroupOwner
	for rows.Next() {
		var o AgentGroupOwner
		var grantedAt string
		if err := rows.Scan(&o.GroupID, &o.AgentID, &o.ConvID, &grantedAt, &o.GrantedBy); err != nil {
			return nil, err
		}
		o.GrantedAt = parseTimeOrZero(grantedAt)
		out = append(out, &o)
	}
	return out, rows.Err()
}

// ListGroupsOwnedBy returns every group_id convID owns. Used by the
// auth check that asks "is the sender an owner of any group the
// target is a member of?".
func ListGroupsOwnedBy(convID string) ([]int64, error) {
	if convID == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return nil, err
	}
	if agentID == "" {
		return nil, nil
	}
	rows, err := d.Query(`SELECT group_id FROM agent_group_owners WHERE agent_id = ?`, agentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CanSenderReachTarget returns the routing group when a message from
// senderID to targetID is authorised, plus a label describing the
// reason (so callers can echo it back to the user). Returns
// (nil, "", nil) when no path exists.
//
// Rules, in order of preference:
//   - shared membership: pick the first group both belong to.
//   - sender-as-owner: pick the first group the sender owns that
//     also contains the target.
//   - via-link: sender is in (or owns) some group A which has an
//     outbound link to a group B that contains the target. The link
//     mode determines whether membership alone suffices or ownership
//     is required (see roleSatisfiesLinkMode).
//
// The order is stable so audit logs ("which path authorised this?")
// keep meaning across versions: tightening the model adds new reasons
// at the end, never reorders the existing ones.
func CanSenderReachTarget(senderID, targetID string) (*AgentGroup, string, error) {
	shared, err := SharedGroupsForConvs(senderID, targetID)
	if err != nil {
		return nil, "", err
	}
	if len(shared) > 0 {
		return shared[0], "shared-group", nil
	}
	ownerGroups, err := ListGroupsOwnedBy(senderID)
	if err != nil {
		return nil, "", err
	}
	for _, gID := range ownerGroups {
		members, err := ListAgentGroupMembers(gID)
		if err != nil {
			continue
		}
		for _, m := range members {
			if m.ConvID == targetID {
				g, err := GetAgentGroupByID(gID)
				if err != nil || g == nil {
					continue
				}
				return g, "owner-of-group", nil
			}
		}
	}
	// via-link: walk outbound link reach and check membership of the
	// target group on the far side.
	reach, err := LinkReachableTargetsFor(senderID)
	if err != nil {
		return nil, "", err
	}
	// Pick the link with the lowest id for determinism — matches the
	// "first by name" convention used by SharedGroupsForConvs.
	var bestVia *AgentGroup
	var bestLinkID int64
	bestReason := ""
	for _, r := range reach {
		members, err := ListAgentGroupMembers(r.Target.ID)
		if err != nil {
			continue
		}
		hit := false
		for _, m := range members {
			if m.ConvID == targetID {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		if bestVia == nil || r.Link.ID < bestLinkID {
			bestVia = r.Via
			bestLinkID = r.Link.ID
			bestReason = fmt.Sprintf("via-link:%d", r.Link.ID)
		}
	}
	if bestVia != nil {
		return bestVia, bestReason, nil
	}
	return nil, "", nil
}

// FindAgentMembersBySelector returns every row whose conv_id matches
// the selector. Used as a fallback by the agent CLI's target resolver:
// a conv that was just added to a group via spawn lives in
// agent_group_members before its .jsonl is scanned into conv_index, so
// the conv_index-only resolver can't find it by title yet — but its
// conv_id is already resolvable here.
//
// Match rules:
//   - exact conv_id
//   - prefix on conv_id (8+ chars typed, like the rest of tclaude)
//
// Returns rows in the order seen; the caller is responsible for
// deduping by conv_id and reporting ambiguity.
func FindAgentMembersBySelector(selector string) ([]*AgentGroupMember, error) {
	if selector == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	// Membership is agent-keyed; match the selector against ANY of the actor's
	// conversation generations (agent_conversations), then return its rows with
	// the actor's current conv for the ConvID field. The IN-subquery (rather
	// than a JOIN on agent_conversations) avoids fan-out when a prefix matches
	// several generations of the same actor.
	q := `SELECT m.group_id, ag.current_conv_id, m.role, m.descr, m.joined_at
		FROM agent_group_members m
		JOIN agents ag ON ag.agent_id = m.agent_id
		WHERE m.agent_id IN (
			SELECT agent_id FROM agent_conversations WHERE conv_id = ? OR conv_id LIKE ?
		)
		ORDER BY m.joined_at DESC`
	rows, err := d.Query(q, selector, selector+"%")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*AgentGroupMember
	for rows.Next() {
		var m AgentGroupMember
		var joined string
		if err := rows.Scan(&m.GroupID, &m.ConvID, &m.Role, &m.Descr, &joined); err != nil {
			return nil, err
		}
		m.JoinedAt = parseTimeOrZero(joined)
		out = append(out, &m)
	}
	return out, rows.Err()
}

// MarkAgentMessageDelivered sets delivered_at = now for the given message ID.
func MarkAgentMessageDelivered(id int64) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE agent_messages SET delivered_at = ?, nudge_claimed_at = '' WHERE id = ?`,
		time.Now().Format(time.RFC3339Nano), id)
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
		WHERE to_conv = ? AND delivered_at = '' AND read_at = '' AND nudge_cancelled_at = ''
		ORDER BY id ASC`
	rows, err := db.Query(q, toConv)
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
		WHERE to_agent = ? AND pin_gen = 0 AND delivered_at = '' AND read_at = '' AND nudge_cancelled_at = ''
		ORDER BY id ASC`
	rows, err := db.Query(q, agentID)
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
		WHERE to_conv = ? AND (pin_gen = 1 OR to_agent = '') AND delivered_at = '' AND read_at = '' AND nudge_cancelled_at = ''
		ORDER BY id ASC`
	rows, err := db.Query(q, convID)
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
		`SELECT COUNT(*) FROM agent_messages WHERE to_agent = ? AND pin_gen = 0 AND delivered_at = '' AND read_at = '' AND nudge_cancelled_at = ''`,
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
		`SELECT COUNT(*) FROM agent_messages WHERE to_conv = ? AND (pin_gen = 1 OR to_agent = '') AND delivered_at = '' AND read_at = '' AND nudge_cancelled_at = ''`,
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
	rows, err := d.Query(`SELECT ` + agentMessageColumns + ` FROM agent_messages
		WHERE delivered_at = '' AND read_at = '' AND nudge_cancelled_at = '' ORDER BY id ASC`)
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
// Ordering is by id (insertion order), NOT created_at, for the same
// RFC3339Nano lexical-sort reason ListUndeliveredAgentMessagesFor documents.
func ListDeliveredUnreadAgentMessages() ([]*AgentMessage, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + agentMessageColumns + `
		FROM agent_messages
		WHERE to_conv != '' AND delivered_at != '' AND read_at = ''
		ORDER BY id ASC`
	rows, err := db.Query(q)
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

type AgentMessageNudgeClaim struct {
	ClaimedAt string
	Attempt   int
}

// ClaimAgentMessageNudge atomically acquires a short-lived nudge lease without
// marking the message delivered. The returned token is the exact timestamp
// plus monotonically-incremented per-message attempt number stored by the same
// UPDATE. Completion/release must present both, so even two RFC3339Nano stamps
// that serialize identically cannot let a stale worker mutate a newer claim.
func ClaimAgentMessageNudge(id int64, now time.Time) (token AgentMessageNudgeClaim, claimed bool, err error) {
	db, err := Open()
	if err != nil {
		return token, false, err
	}
	token.ClaimedAt = now.Format(time.RFC3339Nano)
	err = db.QueryRow(
		`UPDATE agent_messages
		 SET nudge_claimed_at = ?, nudge_attempted_at = ?, nudge_attempts = nudge_attempts + 1
		 WHERE id = ? AND delivered_at = '' AND read_at = '' AND nudge_claimed_at = '' AND nudge_cancelled_at = ''
		 RETURNING nudge_attempts`,
		token.ClaimedAt, token.ClaimedAt, id).Scan(&token.Attempt)
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
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE agent_messages
		SET delivered_at = ?, nudge_claimed_at = ''
		WHERE id = ? AND delivered_at = '' AND nudge_claimed_at = ? AND nudge_attempts = ?`,
		now.Format(time.RFC3339Nano), id, token.ClaimedAt, token.Attempt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ReleaseAgentMessageNudge releases token's lease after a failed send while
// preserving its durable attempt count/time for retry backoff.
func ReleaseAgentMessageNudge(id int64, token AgentMessageNudgeClaim) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE agent_messages SET nudge_claimed_at = ''
		WHERE id = ? AND delivered_at = '' AND nudge_claimed_at = ? AND nudge_attempts = ?`,
		id, token.ClaimedAt, token.Attempt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
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
	res, err := d.Exec(`UPDATE agent_messages SET nudge_claimed_at = ''
		WHERE delivered_at = '' AND nudge_claimed_at != ''`)
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
		WHERE id = ? AND delivered_at = '' AND read_at = ''
		  AND nudge_claimed_at = '' AND nudge_cancelled_at = ''
		  AND NOT EXISTS (
			SELECT 1 FROM agents WHERE agent_id = ? AND retired_at = '')`,
		now.Format(time.RFC3339Nano), reason, id, targetAgentID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// MarkAgentMessageRead sets read_at = now for the given message ID.
func MarkAgentMessageRead(id int64) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE agent_messages SET read_at = ? WHERE id = ?`,
		time.Now().Format(time.RFC3339Nano), id)
	return err
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
	return listAgentMessagesForActor("to_conv", "to_agent", toConv, toAgent, limit)
}

// ListOutboxForActor is the actor-keyed outbox twin of ListInboxForActor:
// every message the caller sent by current conv (from_conv) OR stable actor
// (from_agent), most recent first.
func ListOutboxForActor(fromConv, fromAgent string, limit int) ([]*AgentMessage, error) {
	return listAgentMessagesForActor("from_conv", "from_agent", fromConv, fromAgent, limit)
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
// input — they are interpolated into SQL. Ordering is by id DESC for the same
// RFC3339Nano lexical-sort reason listAgentMessagesByCol documents.
func listAgentMessagesForActor(convCol, agentCol, conv, agent string, limit int) ([]*AgentMessage, error) {
	where, args := actorMatchClause([2]string{convCol, conv}, [2]string{agentCol, agent})
	if where == "" {
		return nil, nil
	}
	db, err := Open()
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + agentMessageColumns + `
		FROM agent_messages WHERE ` + where + ` ORDER BY id DESC`
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

// listAgentMessagesByCol shares the read/scan path between the inbox
// and outbox queries. col must be a literal column name (`to_conv`,
// `from_conv`, `to_agent` or `from_agent`), never user input — it's
// interpolated into SQL.
//
// Ordering is by id DESC (autoincrement = insertion order), NOT created_at.
// created_at is an RFC3339Nano string compared lexically by SQLite: a time on
// a whole second serialises with no fractional part ("…:00Z") and sorts AFTER
// a later same-second value ("…:00.004Z") because '.' < 'Z'. ORDER BY
// created_at could therefore return a newer row as "older" — and with LIMIT,
// silently drop the genuinely-newest row. This is the same RFC3339Nano flake
// already fixed for the undelivered-queue query in #242 (see
// ListUndeliveredAgentMessagesFor); id is monotonic with insertion, giving a
// correct, total most-recent-first order independent of the timestamp format.
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

	// bump records the newest message instant seen for a conv. created_at
	// is stored as zero-padded RFC3339Nano text, so MAX() over it is a
	// valid chronological max; parse it once here.
	bump := func(conv, maxCreated string) {
		c := out[conv]
		if t := parseTimeOrZero(maxCreated); t.After(c.Last) {
			c.Last = t
		}
		out[conv] = c
	}

	// Received side: total in + unread + newest, grouped by recipient.
	inRows, err := db.Query(`SELECT to_conv,
		COUNT(*),
		SUM(CASE WHEN read_at = '' THEN 1 ELSE 0 END),
		MAX(created_at)
		FROM agent_messages WHERE to_conv != '' GROUP BY to_conv`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = inRows.Close() }()
	for inRows.Next() {
		var conv, maxCreated string
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
		var conv, maxCreated string
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

// GroupMessageParticipants returns the distinct conv-ids that ever sent or
// received a group-routed message for groupID — any agent_messages row with
// group_id = groupID, taking both from_conv and to_conv (blank ids, e.g. a
// multicast's empty to_conv, are dropped). It is the durable record of who
// communicated *through* a group: unlike the agent_group_members rows, which
// retire hard-deletes (retireAgentConv unjoins every group before flipping
// the enrollment bit), a message's group_id survives the membership's
// removal. The dashboard uses it to reconstruct a retired ex-member so its
// folder can still nest under the group it used to belong to.
func GroupMessageParticipants(groupID int64) ([]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`
		SELECT from_conv FROM agent_messages WHERE group_id = ? AND from_conv != ''
		UNION
		SELECT to_conv   FROM agent_messages WHERE group_id = ? AND to_conv   != ''`,
		groupID, groupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var conv string
		if err := rows.Scan(&conv); err != nil {
			return nil, err
		}
		out = append(out, conv)
	}
	return out, rows.Err()
}

// MailboxFilter selects which agent_messages rows a dashboard mailbox
// read returns — the backing query for the Messages tab's paginated,
// searchable folder view.
//
// ForConv scopes to one mailbox: rows where the conv is sender OR
// recipient. "" is the virtual "all" firehose (no scope). A single
// (to_conv = ? OR from_conv = ?) predicate dedups the rare self-addressed
// row for free, so the scope needs no UNION.
//
// The remaining fields are the Messages-tab search box, already split
// into the parts the DB can match directly and the parts the caller
// resolved out-of-band:
//   - Text: matched case-insensitively (SQLite LIKE) against subject /
//     body / from_conv / to_conv. A conv-id prefix the operator types
//     matches the full id this way (so the sidebar's short-id is
//     searchable here too).
//   - TitleConvs: convs whose resolved DISPLAY title contained the query.
//     The display title (custom title > pending name > summary > first
//     prompt) is not a single column — the caller resolves it via the
//     conv index and passes the matching conv-ids, which fold in as
//     from_conv/to_conv IN (…).
//   - GroupIDs: groups whose name contained the query, folded in as
//     group_id IN (…).
//
// ExcludeConvs drops any row whose sender OR recipient is one of the
// listed convs — folded in as `(from_conv NOT IN (…) OR subject =
// 'reincarnation handoff') AND to_conv NOT IN (…)`. The dashboard's "all"
// firehose uses it to omit retired agents' traffic unless the operator opts
// in; a 1:1 to/from a retired agent disappears, while a group broadcast
// (empty to_conv) survives unless its own sender is retired. The lone
// carve-out is the reincarnation handoff: its sender is the retired
// predecessor by construction, so the sender side relaxes to "live OR a
// handoff" to keep the live successor's birth record visible (the recipient
// side still must be live). It is an AND constraint independent of the
// (OR-ed) search predicate, so it narrows whatever the search matched.
//
// A filter with empty Text and nil id-sets matches the whole scope (the
// unfiltered total).
//
// ScopeConvs + ScopeGroupID select a GROUP folder — the Messages-tab
// "view this group's messages" view (all member traffic). A row is in
// scope when ANY of: its sender is a member, its recipient is a member,
// or it is one of the group's own multicasts (group_id = ScopeGroupID).
// That OR-set is the group analogue of ForConv's single-conv predicate —
// a group folder is the union of its members' folders, plus the group
// channel. ANDed with search / exclude like ForConv. A group folder sets
// these and leaves ForConv empty; the two are mutually-exclusive folder
// scopes.
type MailboxFilter struct {
	ForConv      string
	ScopeConvs   []string
	ScopeGroupID int64
	Text         string
	TitleConvs   []string
	GroupIDs     []int64
	ExcludeConvs []string
}

// HasSearch reports whether f carries any search predicate (free text or
// a resolved title/group match set) beyond its folder scope.
func (f MailboxFilter) HasSearch() bool {
	return f.Text != "" || len(f.TitleConvs) > 0 || len(f.GroupIDs) > 0
}

// likeEscape escapes the LIKE metacharacters so the operator's search
// text matches literally — a typed '%' means a percent sign, not "any
// run". Pairs with `LIKE ? ESCAPE '\'`.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

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
	// Group folder scope: a row is in scope when a member is sender or
	// recipient, or it is one of the group's own multicasts. Built as its
	// own OR-group so it ANDs with search / exclude just like ForConv. A
	// group with no members still scopes to its channel (group_id only).
	if len(f.ScopeConvs) > 0 || f.ScopeGroupID != 0 {
		var ors []string
		if len(f.ScopeConvs) > 0 {
			ph := sqlPlaceholders(len(f.ScopeConvs))
			ors = append(ors, "from_conv IN ("+ph+")", "to_conv IN ("+ph+")")
			for _, c := range f.ScopeConvs {
				args = append(args, c)
			}
			for _, c := range f.ScopeConvs {
				args = append(args, c)
			}
		}
		if f.ScopeGroupID != 0 {
			ors = append(ors, "group_id = ?")
			args = append(args, f.ScopeGroupID)
		}
		clauses = append(clauses, "("+strings.Join(ors, " OR ")+")")
	}
	if f.HasSearch() {
		var ors []string
		if f.Text != "" {
			like := "%" + likeEscape(f.Text) + "%"
			ors = append(ors,
				`subject LIKE ? ESCAPE '\'`,
				`body LIKE ? ESCAPE '\'`,
				`from_conv LIKE ? ESCAPE '\'`,
				`to_conv LIKE ? ESCAPE '\'`)
			args = append(args, like, like, like, like)
		}
		if len(f.TitleConvs) > 0 {
			ph := sqlPlaceholders(len(f.TitleConvs))
			ors = append(ors, "from_conv IN ("+ph+")", "to_conv IN ("+ph+")")
			for _, c := range f.TitleConvs {
				args = append(args, c)
			}
			for _, c := range f.TitleConvs {
				args = append(args, c)
			}
		}
		if len(f.GroupIDs) > 0 {
			ph := sqlPlaceholders(len(f.GroupIDs))
			ors = append(ors, "group_id IN ("+ph+")")
			for _, g := range f.GroupIDs {
				args = append(args, g)
			}
		}
		if len(ors) > 0 {
			clauses = append(clauses, "("+strings.Join(ors, " OR ")+")")
		}
	}
	if len(f.ExcludeConvs) > 0 {
		ph := sqlPlaceholders(len(f.ExcludeConvs))
		// AND, not OR: a row survives only when NEITHER party is excluded —
		// with one carve-out. A reincarnation handoff is the bridge record
		// from a retiring predecessor to its live successor, so its SENDER is
		// always retired (and thus excluded). Dropping it would hide the
		// successor's own birth certificate from the firehose / group folder
		// even though those rows belong to a live agent (the per-agent folder
		// already shows them, having no exclude). So the sender side relaxes
		// to "live OR a handoff", while the recipient side still must be live
		// — a handoff whose successor has itself since retired is fully
		// historical and stays hidden, and a live→retired DM still drops.
		clauses = append(clauses,
			"(from_conv NOT IN ("+ph+") OR subject = ?)",
			"to_conv NOT IN ("+ph+")")
		for _, c := range f.ExcludeConvs {
			args = append(args, c)
		}
		args = append(args, ReincarnationHandoffSubject)
		for _, c := range f.ExcludeConvs {
			args = append(args, c)
		}
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return strings.Join(clauses, " AND "), args
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
// 0. Ordered by id DESC (insertion order), not created_at, for the same
// RFC3339Nano lexical-sort reason listAgentMessagesByCol documents.
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

// DistinctAgentMessageConvs returns every conv-id that appears as a
// sender or recipient in agent_messages. Small — bounded by the number
// of agents that ever exchanged mail, not the message count — so the
// dashboard can resolve each one's display title in Go and decide which
// match the mailbox search box (feeding MailboxFilter.TitleConvs)
// without a conv_index join.
func DistinctAgentMessageConvs() ([]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT from_conv FROM agent_messages WHERE from_conv != ''
		UNION
		SELECT to_conv FROM agent_messages WHERE to_conv != ''`)
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
// DeleteAgentMessagesByIDs. Only rows that actually change state are touched,
// so marking-read leaves an already-read row's read_at timestamp intact and
// the returned count reflects real transitions (mirroring the idempotent
// no-op the batched UI relies on). A non-existent id is a silent skip.
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
	args := make([]any, 0, len(ids)+1)
	// When marking read the timestamp is the first bound parameter (it
	// precedes the IN-list in the UPDATE); marking unread binds the ids only.
	if read {
		args = append(args, time.Now().Format(time.RFC3339Nano))
	}
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	in := strings.Join(placeholders, ",")
	var q string
	if read {
		q = `UPDATE agent_messages SET read_at = ? WHERE read_at = '' AND id IN (` + in + `)`
	} else {
		q = `UPDATE agent_messages SET read_at = '' WHERE read_at != '' AND id IN (` + in + `)`
	}
	res, err := d.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// MarkAgentMailboxRead marks every still-unread message a conv has RECEIVED
// (to_conv = conv, read_at == ”) as read, stamping read_at=now. It backs the
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
	res, err := d.Exec(
		`UPDATE agent_messages SET read_at = ? WHERE to_conv = ? AND read_at = ''`,
		time.Now().Format(time.RFC3339Nano), conv)
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

func formatTimeOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
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
	t, err := time.Parse(time.RFC3339Nano, s)
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
	var createdAt, archivedAt string
	var remoteControl, parentID sql.NullInt64
	if err := s.Scan(&g.ID, &g.Name, &g.Descr, &g.DefaultCwd, &g.DefaultContext,
		&g.DefaultProfile, &g.SandboxProfile, &g.SandboxProfileID,
		&g.MaxMembers, &g.NotifyEnabled, &remoteControl, &g.Mission,
		&g.SourceTemplate, &g.SourceTemplateID, &createdAt, &archivedAt, &parentID); err != nil {
		return nil, err
	}
	g.RemoteControl = nullToBoolPtr(remoteControl)
	g.ParentGroupID = nullToInt64Ptr(parentID)
	g.CreatedAt = parseTimeOrZero(createdAt)
	g.ArchivedAt = parseTimeOrZero(archivedAt)
	return &g, nil
}

func scanAgentGroupMember(s rowScanner) (*AgentGroupMember, error) {
	var m AgentGroupMember
	var joinedAt string
	if err := s.Scan(&m.GroupID, &m.ConvID, &m.Role, &m.Descr, &joinedAt); err != nil {
		return nil, err
	}
	m.JoinedAt = parseTimeOrZero(joinedAt)
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
	`nudge_cancelled_at, nudge_cancel_reason`

func scanAgentMessage(s rowScanner) (*AgentMessage, error) {
	var m AgentMessage
	var createdAt, deliveredAt, readAt, nudgeClaimedAt, nudgeAttemptedAt string
	var nudgeCancelledAt string
	var toRecipients, ccRecipients string
	var toRecipientAgents, ccRecipientAgents string
	var pinGen int
	if err := s.Scan(&m.ID, &m.GroupID, &m.FromConv, &m.ToConv,
		&m.FromAgent, &m.ToAgent,
		&m.Subject, &m.Body, &m.ParentID,
		&createdAt, &deliveredAt, &readAt,
		&toRecipients, &ccRecipients,
		&toRecipientAgents, &ccRecipientAgents, &m.OriginalToConv, &pinGen,
		&nudgeClaimedAt, &nudgeAttemptedAt, &m.NudgeAttempts,
		&nudgeCancelledAt, &m.NudgeCancelReason); err != nil {
		return nil, err
	}
	m.CreatedAt = parseTimeOrZero(createdAt)
	m.DeliveredAt = parseTimeOrZero(deliveredAt)
	m.ReadAt = parseTimeOrZero(readAt)
	m.NudgeClaimedAt = parseTimeOrZero(nudgeClaimedAt)
	m.NudgeAttemptedAt = parseTimeOrZero(nudgeAttemptedAt)
	m.NudgeCancelledAt = parseTimeOrZero(nudgeCancelledAt)
	m.ToRecipients = recipientsFromJSON(toRecipients)
	m.CcRecipients = recipientsFromJSON(ccRecipients)
	m.ToRecipientAgents = recipientsFromJSON(toRecipientAgents)
	m.CcRecipientAgents = recipientsFromJSON(ccRecipientAgents)
	m.PinGen = pinGen != 0
	return &m, nil
}
