package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AgentGroup is a row in agent_groups.
type AgentGroup struct {
	ID        int64
	Name      string
	Descr     string
	CreatedAt time.Time
}

// AgentGroupMember is a row in agent_group_members.
type AgentGroupMember struct {
	GroupID  int64
	ConvID   string
	Alias    string
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
	return err
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

// AgentMessage is a row in agent_messages. Body is stored inline.
// ParentID is the message this one is a reply to, or 0 for top-of-thread.
type AgentMessage struct {
	ID          int64
	GroupID     int64
	FromConv    string
	ToConv      string
	Subject     string
	Body        string
	ParentID    int64
	CreatedAt   time.Time
	DeliveredAt time.Time
	ReadAt      time.Time
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

// DeleteAgentGroup removes a group by name. Cascades to members; returns
// an error if any messages still reference the group (ON DELETE RESTRICT).
func DeleteAgentGroup(name string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM agent_groups WHERE name = ?`, name)
	return err
}

// GetAgentGroupByName returns the group with the given name, or nil if not
// found.
func GetAgentGroupByName(name string) (*AgentGroup, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(`SELECT id, name, descr, created_at FROM agent_groups WHERE name = ?`, name)
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
	rows, err := db.Query(`SELECT id, name, descr, created_at FROM agent_groups ORDER BY name`)
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
		(group_id, conv_id, alias, role, descr, joined_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		m.GroupID, m.ConvID, m.Alias, m.Role, m.Descr,
		m.JoinedAt.Format(time.RFC3339Nano))
	return err
}

// UpdateAgentGroupMember patches non-nil fields on an existing member.
// Pass a nil pointer for any field you don't want to change. Returns
// (rowsAffected, error); 0 rows means no such (group, conv) pair.
func UpdateAgentGroupMember(groupID int64, convID string, alias, role, descr *string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	sets := []string{}
	args := []any{}
	if alias != nil {
		sets = append(sets, "alias = ?")
		args = append(args, *alias)
	}
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

// ListAgentGroupMembers returns the members of a group, ordered by alias
// then conv_id.
func ListAgentGroupMembers(groupID int64) ([]*AgentGroupMember, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT group_id, conv_id, alias, role, descr, joined_at
		FROM agent_group_members WHERE group_id = ? ORDER BY alias, conv_id`, groupID)
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
	rows, err := db.Query(`SELECT g.id, g.name, g.descr, g.created_at
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
	rows, err := db.Query(`SELECT g.id, g.name, g.descr, g.created_at
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
	row := db.QueryRow(`SELECT group_id, conv_id, alias, role, descr, joined_at
		FROM agent_group_members WHERE group_id = ? AND conv_id = ?`, groupID, convID)
	m, err := scanAgentGroupMember(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// InsertAgentMessage records a delivered message and returns its ID.
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
		 created_at, delivered_at, read_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.GroupID, m.FromConv, m.ToConv, m.Subject, m.Body, m.ParentID,
		m.CreatedAt.Format(time.RFC3339Nano),
		formatTimeOrEmpty(m.DeliveredAt), formatTimeOrEmpty(m.ReadAt))
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

// GetAgentMessage returns a single message by ID, or nil if not found.
func GetAgentMessage(id int64) (*AgentMessage, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(`SELECT id, group_id, from_conv, to_conv, subject, body, parent_id,
		created_at, delivered_at, read_at
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
		created_at, delivered_at, read_at
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
	var createdAt string
	if err := s.Scan(&g.ID, &g.Name, &g.Descr, &createdAt); err != nil {
		return nil, err
	}
	g.CreatedAt = parseTimeOrZero(createdAt)
	return &g, nil
}

func scanAgentGroupMember(s rowScanner) (*AgentGroupMember, error) {
	var m AgentGroupMember
	var joinedAt string
	if err := s.Scan(&m.GroupID, &m.ConvID, &m.Alias, &m.Role, &m.Descr, &joinedAt); err != nil {
		return nil, err
	}
	m.JoinedAt = parseTimeOrZero(joinedAt)
	return &m, nil
}

func scanAgentMessage(s rowScanner) (*AgentMessage, error) {
	var m AgentMessage
	var createdAt, deliveredAt, readAt string
	if err := s.Scan(&m.ID, &m.GroupID, &m.FromConv, &m.ToConv,
		&m.Subject, &m.Body, &m.ParentID,
		&createdAt, &deliveredAt, &readAt); err != nil {
		return nil, err
	}
	m.CreatedAt = parseTimeOrZero(createdAt)
	m.DeliveredAt = parseTimeOrZero(deliveredAt)
	m.ReadAt = parseTimeOrZero(readAt)
	return &m, nil
}
