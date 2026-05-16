package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AgentGroup is a row in agent_groups.
type AgentGroup struct {
	ID             int64
	Name           string
	Descr          string
	DefaultCwd     string // pre-filled cwd for agents spawned into this group; "" = none
	DefaultContext string // shared startup context delivered to the inbox of agents spawned into this group; "" = none
	MaxMembers     int    // hard cap on member count; a spawn that would exceed it is refused. 0 = unlimited
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

// AgentPermission is a row in agent_permissions — a per-conv grant of
// a permission slug. Lives in SQLite (rather than ~/.tclaude/config.json)
// so the daemon can grant/revoke without JSON-file rewrites.
// DefaultPermissions for *all* agents still live in config.json, since
// those describe baseline trust the human curates explicitly.
type AgentPermission struct {
	ConvID    string
	Slug      string
	GrantedAt time.Time
	GrantedBy string
}

// HasAgentPermissionRow checks whether (convID, slug) is granted in the
// agent_permissions table. Used by the daemon's per-agent override
// lookup. Errors propagate so the caller can refuse rather than silently
// allow.
func HasAgentPermissionRow(convID, slug string) (bool, error) {
	db, err := Open()
	if err != nil {
		return false, err
	}
	var n int
	err = db.QueryRow(`SELECT COUNT(*) FROM agent_permissions WHERE conv_id = ? AND slug = ?`,
		convID, slug).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListAgentPermissionsForConv returns every slug granted to this conv,
// alphabetised. Returns an empty slice (not nil) for a conv with no grants.
func ListAgentPermissionsForConv(convID string) ([]string, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT slug FROM agent_permissions WHERE conv_id = ? ORDER BY slug`, convID)
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

// ListAllAgentPermissions returns the full grant table, grouped by conv-id.
// Convs with no grants are absent from the map. Used by the dashboard /
// permissions ls view.
func ListAllAgentPermissions() (map[string][]string, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT conv_id, slug FROM agent_permissions ORDER BY conv_id, slug`)
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

// GrantAgentPermission inserts (convID, slug). Idempotent — running
// twice is a no-op. grantedBy is informational ("<human>" or a
// granter's conv-id); empty is fine.
func GrantAgentPermission(convID, slug, grantedBy string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT OR IGNORE INTO agent_permissions
		(conv_id, slug, granted_at, granted_by)
		VALUES (?, ?, ?, ?)`,
		convID, slug, time.Now().Format(time.RFC3339Nano), grantedBy)
	if err != nil {
		return err
	}
	// Holding a permission grant makes the conv an agent.
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
	row := db.QueryRow(`SELECT id, name, descr, default_cwd, default_context, max_members, created_at, archived_at FROM agent_groups WHERE name = ?`, name)
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
	row := db.QueryRow(`SELECT id, name, descr, default_cwd, default_context, max_members, created_at, archived_at FROM agent_groups WHERE id = ?`, id)
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
	rows, err := db.Query(`SELECT id, name, descr, default_cwd, default_context, max_members, created_at, archived_at FROM agent_groups ORDER BY name`)
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
		`SELECT id, name, descr, default_cwd, default_context, max_members, created_at, archived_at FROM agent_groups WHERE name = ?`,
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
	rows, err := db.Query(`SELECT g.id, g.name, g.descr, g.default_cwd, g.default_context, g.max_members, g.created_at, g.archived_at
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
	rows, err := db.Query(`SELECT g.id, g.name, g.descr, g.default_cwd, g.default_context, g.max_members, g.created_at, g.archived_at
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
		ORDER BY created_at ASC`
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
func listAgentMessagesByCol(col, value string, limit int) ([]*AgentMessage, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	q := `SELECT id, group_id, from_conv, to_conv, subject, body, parent_id,
		created_at, delivered_at, read_at,
		to_recipients, cc_recipients, original_to_conv
		FROM agent_messages WHERE ` + col + ` = ? ORDER BY created_at DESC`
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
	if err := s.Scan(&g.ID, &g.Name, &g.Descr, &g.DefaultCwd, &g.DefaultContext, &g.MaxMembers, &createdAt, &archivedAt); err != nil {
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
