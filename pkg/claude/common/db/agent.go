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
	ID             int64
	Name           string
	Descr          string
	DefaultCwd     string // pre-filled cwd for agents spawned into this group; "" = none
	DefaultContext string // shared startup context delivered to the inbox of agents spawned into this group; "" = none
	DefaultProfile string // name of the spawn profile whose launch fields fill blank spawn fields server-side (JOH-210); "" = none
	MaxMembers     int    // hard cap on member count; a spawn that would exceed it is refused. 0 = unlimited
	NotifyEnabled  bool   // OS notifications for member agents; false mutes the whole group (a per-agent 'on' pref still overrides)
	CreatedAt      time.Time
	ArchivedAt     time.Time // zero = active; non-zero = archived (soft-deleted)
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
	var n int
	err = db.QueryRow(`SELECT COUNT(*) FROM agent_permissions WHERE conv_id = ? AND slug = ? AND effect = ?`,
		convID, slug, PermEffectGrant).Scan(&n)
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
	err = db.QueryRow(`SELECT effect FROM agent_permissions WHERE conv_id = ? AND slug = ?`,
		convID, slug).Scan(&effect)
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
	rows, err := db.Query(`SELECT slug FROM agent_permissions WHERE conv_id = ? AND effect = ? ORDER BY slug`,
		convID, PermEffectGrant)
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
	rows, err := db.Query(`SELECT slug, effect FROM agent_permissions WHERE conv_id = ? ORDER BY slug`, convID)
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
	rows, err := db.Query(`SELECT conv_id, slug FROM agent_permissions WHERE effect = ? ORDER BY conv_id, slug`,
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
	rows, err := db.Query(`SELECT conv_id, slug, effect FROM agent_permissions ORDER BY conv_id, slug`)
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
	_, err = db.Exec(`INSERT INTO agent_permissions
		(conv_id, slug, effect, granted_at, granted_by)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(conv_id, slug) DO UPDATE SET
			effect     = excluded.effect,
			granted_at = excluded.granted_at,
			granted_by = excluded.granted_by`,
		convID, slug, effect, time.Now().Format(time.RFC3339Nano), grantedBy)
	if err != nil {
		return err
	}
	// Holding a permission override (grant or deny) makes the conv an
	// agent — a deny is still per-agent permission config.
	return EnrollAgent(convID, "grant")
}

// RevokeAgentPermission removes a single (convID, slug). Idempotent.
// Returns the number of rows deleted (0 if there was nothing to remove).
func RevokeAgentPermission(convID, slug string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`DELETE FROM agent_permissions WHERE conv_id = ? AND slug = ?`, convID, slug)
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
	res, err := db.Exec(`DELETE FROM agent_permissions WHERE conv_id = ?`, convID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// AgentMessage is a row in agent_messages. Body is stored inline.
// ParentID is the message this one is a reply to, or 0 for top-of-thread.
//
// ToRecipients / CcRecipients are the email-style audience of the
// original send: every row of a multi-recipient send carries the same
// arrays (denormalized) so each recipient knows who else got the
// message. Empty for legacy single-recipient sends — ToConv stays
// canonical for delivery + filtering, the recipient arrays are
// display-only.
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
	Subject        string
	Body           string
	ParentID       int64
	CreatedAt      time.Time
	DeliveredAt    time.Time
	ReadAt         time.Time
	ToRecipients   []string
	CcRecipients   []string
}

// CreateAgentGroup inserts a new group. Returns the new group's ID.
func CreateAgentGroup(name, descr string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`INSERT INTO agent_groups (name, descr, created_at) VALUES (?, ?, ?)`,
		name, descr, time.Now().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
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
	res, err := db.Exec(`
		INSERT INTO agent_groups
			(name, descr, default_cwd, default_context, default_profile,
			 max_members, notify_enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		name, src.Descr, src.DefaultCwd, src.DefaultContext, src.DefaultProfile,
		src.MaxMembers, src.NotifyEnabled, time.Now().Format(time.RFC3339Nano))
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
// for agents spawned into the named group (JOH-210). The caller validates
// that the referenced profile exists before it gets here. Returns the
// number of rows affected — 0 means no group by that name, so the caller
// can answer 404.
func SetAgentGroupDefaultProfile(name, profile string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`UPDATE agent_groups SET default_profile = ? WHERE name = ?`, profile, name)
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
	if _, err := tx.Exec(`DELETE FROM agent_groups WHERE id = ?`, gID); err != nil {
		return err
	}
	return tx.Commit()
}

// GetAgentGroupByName returns the group with the given name, or nil if not
// found.
func GetAgentGroupByName(name string) (*AgentGroup, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(`SELECT id, name, descr, default_cwd, default_context, default_profile, max_members, notify_enabled, created_at, archived_at FROM agent_groups WHERE name = ?`, name)
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
	row := db.QueryRow(`SELECT id, name, descr, default_cwd, default_context, default_profile, max_members, notify_enabled, created_at, archived_at FROM agent_groups WHERE id = ?`, id)
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
	rows, err := db.Query(`SELECT id, name, descr, default_cwd, default_context, default_profile, max_members, notify_enabled, created_at, archived_at FROM agent_groups ORDER BY name`)
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
		`SELECT id, name, descr, default_cwd, default_context, default_profile, max_members, notify_enabled, created_at, archived_at FROM agent_groups WHERE name = ?`,
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
			`INSERT INTO agent_group_audit (group_id, old_name, new_name, by_conv, at)
			 VALUES (?, ?, ?, ?, ?)`,
			g.ID, oldName, newName, byConv, time.Now().Format(time.RFC3339Nano)); err != nil {
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
		`INSERT INTO agent_group_audit (group_id, old_name, new_name, by_conv, at)
		 VALUES (?, ?, ?, ?, ?)`,
		g.ID, oldName, newName, byConv, time.Now().Format(time.RFC3339Nano)); err != nil {
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
		`SELECT id, group_id, old_name, new_name, by_conv, at
		 FROM agent_group_audit WHERE group_id = ? ORDER BY id DESC`, groupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AgentGroupAudit
	for rows.Next() {
		var a AgentGroupAudit
		if err := rows.Scan(&a.ID, &a.GroupID, &a.OldName, &a.NewName, &a.ByConv, &a.At); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AgentGroupAudit is one row in agent_group_audit, recording a single
// rename event.
type AgentGroupAudit struct {
	ID      int64
	GroupID int64
	OldName string
	NewName string
	ByConv  string
	At      string
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

// AddAgentGroupMember inserts (or replaces) a member in a group.
func AddAgentGroupMember(m *AgentGroupMember) error {
	db, err := Open()
	if err != nil {
		return err
	}
	if m.JoinedAt.IsZero() {
		m.JoinedAt = time.Now()
	}
	_, err = db.Exec(`INSERT OR REPLACE INTO agent_group_members
		(group_id, conv_id, role, descr, joined_at)
		VALUES (?, ?, ?, ?, ?)`,
		m.GroupID, m.ConvID, m.Role, m.Descr,
		m.JoinedAt.Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	// Joining a group makes the conv an agent. Insert-only — a stray
	// add never un-retires; the dashboard add-member flow reinstates
	// retired targets explicitly.
	return EnrollAgent(m.ConvID, "group")
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
	args = append(args, groupID, convID)
	q := "UPDATE agent_group_members SET " + sets[0]
	for i := 1; i < len(sets); i++ {
		q += ", " + sets[i]
	}
	q += " WHERE group_id = ? AND conv_id = ?"
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
	_, err = db.Exec(`DELETE FROM agent_group_members WHERE group_id = ? AND conv_id = ?`,
		groupID, convID)
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
	Enrollment     int64 `json:"enrollment"`
	NotifyPrefs    int64 `json:"notify_prefs"`
}

// DeleteAgentByConvID purges every row that references convID across
// the agent / conv / session tables. Single transaction — partial
// failure rolls everything back.
//
// Tables hit (in dependency order):
//
//   - agent_group_members
//   - agent_group_owners
//   - agent_messages (from_conv = ? OR to_conv = ?)
//   - agent_permissions
//   - agent_cron_jobs (owner_conv = ? OR target_conv = ?). Cascades
//     to agent_cron_runs via the FK.
//   - agent_conv_succession (old_conv_id = ? OR new_conv_id = ?).
//     Both sides — chain history of the deleted agent disappears.
//   - conv_embeddings
//   - conv_index
//   - sessions
//   - agent_enrollment
//   - agent_notify_prefs
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

	type step struct {
		stmt string
		into *int64
	}
	steps := []step{
		{`DELETE FROM agent_group_members WHERE conv_id = ?`, &c.GroupMembers},
		{`DELETE FROM agent_group_owners WHERE conv_id = ?`, &c.GroupOwners},
		{`DELETE FROM agent_messages WHERE from_conv = ?`, &c.MessagesFrom},
		{`DELETE FROM agent_messages WHERE to_conv = ?`, &c.MessagesTo},
		{`DELETE FROM agent_permissions WHERE conv_id = ?`, &c.Permissions},
		{`DELETE FROM agent_cron_jobs WHERE owner_conv = ?`, &c.CronJobsOwned},
		{`DELETE FROM agent_cron_jobs WHERE target_conv = ?`, &c.CronJobsTarget},
		{`DELETE FROM agent_conv_succession WHERE old_conv_id = ?`, &c.SuccessionOld},
		{`DELETE FROM agent_conv_succession WHERE new_conv_id = ?`, &c.SuccessionNew},
		{`DELETE FROM conv_embeddings WHERE conv_id = ?`, &c.Embeddings},
		{`DELETE FROM conv_index WHERE conv_id = ?`, &c.ConvIndex},
		{`DELETE FROM sessions WHERE conv_id = ?`, &c.Sessions},
		{`DELETE FROM agent_enrollment WHERE conv_id = ?`, &c.Enrollment},
		{`DELETE FROM agent_notify_prefs WHERE conv_id = ?`, &c.NotifyPrefs},
	}
	for _, s := range steps {
		res, err := tx.Exec(s.stmt, convID)
		if err != nil {
			return AgentDeletionCounts{}, fmt.Errorf("delete agent (%s): %w", s.stmt, err)
		}
		n, _ := res.RowsAffected()
		*s.into = n
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
	res, err := db.Exec(`DELETE FROM agent_group_members WHERE conv_id = ?`, convID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListAgentGroupMembers returns the members of a group, ordered by
// joined_at then conv_id.
func ListAgentGroupMembers(groupID int64) ([]*AgentGroupMember, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT group_id, conv_id, role, descr, joined_at
		FROM agent_group_members WHERE group_id = ? ORDER BY joined_at, conv_id`, groupID)
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
// ordered by group name.
func ListGroupsForConv(convID string) ([]*AgentGroup, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT g.id, g.name, g.descr, g.default_cwd, g.default_context, g.default_profile, g.max_members, g.notify_enabled, g.created_at, g.archived_at
		FROM agent_groups g
		JOIN agent_group_members m ON m.group_id = g.id
		WHERE m.conv_id = ?
		ORDER BY g.name`, convID)
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
	rows, err := db.Query(`SELECT m.conv_id, g.name
		FROM agent_group_members m
		JOIN agent_groups g ON g.id = m.group_id
		ORDER BY m.conv_id, g.name`)
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
	rows, err := db.Query(`SELECT g.id, g.name, g.descr, g.default_cwd, g.default_context, g.default_profile, g.max_members, g.notify_enabled, g.created_at, g.archived_at
		FROM agent_groups g
		JOIN agent_group_members ma ON ma.group_id = g.id AND ma.conv_id = ?
		JOIN agent_group_members mb ON mb.group_id = g.id AND mb.conv_id = ?
		ORDER BY g.name`, a, b)
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
	row := db.QueryRow(`SELECT group_id, conv_id, role, descr, joined_at
		FROM agent_group_members WHERE group_id = ? AND conv_id = ?`, groupID, convID)
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
	res, err := db.Exec(`INSERT INTO agent_messages
		(group_id, from_conv, to_conv, subject, body, parent_id,
		 created_at, delivered_at, read_at,
		 to_recipients, cc_recipients, original_to_conv)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.GroupID, m.FromConv, m.ToConv, m.Subject, m.Body, m.ParentID,
		m.CreatedAt.Format(time.RFC3339Nano),
		formatTimeOrEmpty(m.DeliveredAt), formatTimeOrEmpty(m.ReadAt),
		recipientsToJSON(m.ToRecipients), recipientsToJSON(m.CcRecipients),
		m.OriginalToConv)
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

// AgentGroupOwner is a row in agent_group_owners. Owners can message
// a group's members and multicast to the group without being members
// themselves. Distinct from membership so the "X is an owner but
// not a peer" case is representable.
type AgentGroupOwner struct {
	GroupID   int64
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
	_, err = d.Exec(
		`INSERT OR IGNORE INTO agent_group_owners (group_id, conv_id, granted_at, granted_by)
		 VALUES (?, ?, ?, ?)`,
		groupID, convID, time.Now().Format(time.RFC3339Nano), grantedBy)
	if err != nil {
		return err
	}
	// Owning a group makes the conv an agent.
	return EnrollAgent(convID, "group")
}

// RemoveAgentGroupOwner clears an ownership row. Returns the number
// of rows removed (0 when convID wasn't an owner).
func RemoveAgentGroupOwner(groupID int64, convID string) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(
		`DELETE FROM agent_group_owners WHERE group_id = ? AND conv_id = ?`,
		groupID, convID)
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
	res, err := d.Exec(`DELETE FROM agent_group_owners WHERE conv_id = ?`, convID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// IsAgentGroupOwner returns true when (groupID, convID) is in
// agent_group_owners.
func IsAgentGroupOwner(groupID int64, convID string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var n int
	err = d.QueryRow(
		`SELECT COUNT(*) FROM agent_group_owners WHERE group_id = ? AND conv_id = ?`,
		groupID, convID).Scan(&n)
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
		`SELECT group_id, conv_id, granted_at, granted_by
		 FROM agent_group_owners WHERE group_id = ?
		 ORDER BY granted_at DESC`,
		groupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*AgentGroupOwner
	for rows.Next() {
		var o AgentGroupOwner
		var grantedAt string
		if err := rows.Scan(&o.GroupID, &o.ConvID, &grantedAt, &o.GrantedBy); err != nil {
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
	rows, err := d.Query(`SELECT group_id FROM agent_group_owners WHERE conv_id = ?`, convID)
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
	q := `SELECT group_id, conv_id, role, descr, joined_at
		FROM agent_group_members
		WHERE conv_id = ?
		   OR conv_id LIKE ?
		ORDER BY joined_at DESC`
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
	_, err = db.Exec(`UPDATE agent_messages SET delivered_at = ? WHERE id = ?`,
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
	q := `SELECT id, group_id, from_conv, to_conv, subject, body, parent_id,
		created_at, delivered_at, read_at,
		to_recipients, cc_recipients, original_to_conv
		FROM agent_messages
		WHERE to_conv = ? AND delivered_at = ''
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

// ClaimAgentMessageDelivery atomically marks a message as delivered
// IF it wasn't already. Returns true when this caller won the race —
// in that case the caller is responsible for actually delivering the
// nudge. Returns false (with no error) when another goroutine got
// there first; the caller must NOT re-deliver.
//
// This is the concurrency primitive that makes the flush-on-online
// path safe: every request that resolves to a conv-id can speculatively
// flush, and only one delivery path will fire per message.
func ClaimAgentMessageDelivery(id int64) (bool, error) {
	db, err := Open()
	if err != nil {
		return false, err
	}
	res, err := db.Exec(
		`UPDATE agent_messages SET delivered_at = ?
		 WHERE id = ? AND delivered_at = ''`,
		time.Now().Format(time.RFC3339Nano), id)
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

// GetAgentMessage returns a single message by ID, or nil if not found.
func GetAgentMessage(id int64) (*AgentMessage, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(`SELECT id, group_id, from_conv, to_conv, subject, body, parent_id,
		created_at, delivered_at, read_at,
		to_recipients, cc_recipients, original_to_conv
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

// listAgentMessagesByCol shares the read/scan path between the inbox
// and outbox queries. col must be a literal column name (`to_conv` or
// `from_conv`), never user input — it's interpolated into SQL.
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
	q := `SELECT id, group_id, from_conv, to_conv, subject, body, parent_id,
		created_at, delivered_at, read_at,
		to_recipients, cc_recipients, original_to_conv
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
// listed convs — folded in as `from_conv NOT IN (…) AND to_conv NOT IN
// (…)`. The dashboard's "all" firehose uses it to omit retired agents'
// traffic unless the operator opts in; a 1:1 to/from a retired agent
// disappears, while a group broadcast (empty to_conv) survives unless
// its own sender is retired. It is an AND constraint independent of the
// (OR-ed) search predicate, so it narrows whatever the search matched.
//
// A filter with empty Text and nil id-sets matches the whole scope (the
// unfiltered total).
type MailboxFilter struct {
	ForConv      string
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
		// AND, not OR: a row survives only when NEITHER party is excluded.
		clauses = append(clauses, "from_conv NOT IN ("+ph+")", "to_conv NOT IN ("+ph+")")
		for _, c := range f.ExcludeConvs {
			args = append(args, c)
		}
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
	q := `SELECT id, group_id, from_conv, to_conv, subject, body, parent_id,
		created_at, delivered_at, read_at,
		to_recipients, cc_recipients, original_to_conv
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
// (to_conv = conv, read_at == '') as read, stamping read_at=now. It backs the
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
	if err := s.Scan(&g.ID, &g.Name, &g.Descr, &g.DefaultCwd, &g.DefaultContext, &g.DefaultProfile, &g.MaxMembers, &g.NotifyEnabled, &createdAt, &archivedAt); err != nil {
		return nil, err
	}
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

func scanAgentMessage(s rowScanner) (*AgentMessage, error) {
	var m AgentMessage
	var createdAt, deliveredAt, readAt string
	var toRecipients, ccRecipients string
	if err := s.Scan(&m.ID, &m.GroupID, &m.FromConv, &m.ToConv,
		&m.Subject, &m.Body, &m.ParentID,
		&createdAt, &deliveredAt, &readAt,
		&toRecipients, &ccRecipients, &m.OriginalToConv); err != nil {
		return nil, err
	}
	m.CreatedAt = parseTimeOrZero(createdAt)
	m.DeliveredAt = parseTimeOrZero(deliveredAt)
	m.ReadAt = parseTimeOrZero(readAt)
	m.ToRecipients = recipientsFromJSON(toRecipients)
	m.CcRecipients = recipientsFromJSON(ccRecipients)
	return &m, nil
}
