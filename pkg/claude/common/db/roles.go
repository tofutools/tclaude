package db

import (
	"database/sql"
	"errors"
	"time"
)

// ErrRoleNameTaken is returned by CreateRole / UpdateRole when another role
// already owns the name. The name is the human-facing handle and the route key
// (/v1/roles/{name}), so it carries a UNIQUE constraint.
var ErrRoleNameTaken = errors.New("a role with that name already exists")

// Role is a row in roles — a named, reusable bundle of defaults a template
// roster agent can reference instead of re-typing them (JOH-240). A role
// carries a canonical role-brief (guidance prepended to that agent's startup
// briefing) and a default permission set. Launch policy deliberately belongs
// to spawn profiles, not roles.
//
// A template agent references a role by name (group_template_agents.role_ref);
// the referenced role supplies behavioral guidance and access defaults while
// the template agent / spawn profile independently supplies its launch shape.
type Role struct {
	ID   int64
	Name string // the role handle (UNIQUE)
	// Descr is a one-line summary shown in the role list.
	Descr string
	// Brief is the canonical role-brief — guidance rendered into a "## Role"
	// block in the composed startup context of any agent referencing this
	// role. "" = no brief (the block is omitted).
	Brief string

	// Permissions is the role's baseline permission set, merged beneath a
	// referencing agent's spawn-profile or explicit permission overrides. Each
	// entry is a slug plus an optional scope. Stored as
	// a JSON list like group_template_agents; a bare slug is unscoped.
	Permissions []PermissionGrant

	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateRole inserts a new role and returns its ID. A name collision surfaces
// as ErrRoleNameTaken.
func CreateRole(rl *Role) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	now := dbTime(time.Now())
	res, err := d.Exec(
		`INSERT INTO roles (name, descr, brief, permissions, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		rl.Name, rl.Descr, rl.Brief, permsToJSON(rl.Permissions), now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrRoleNameTaken
		}
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateRole rewrites an existing role identified by rl.ID. Renaming to a name
// another role holds surfaces as ErrRoleNameTaken; a missing ID returns
// sql.ErrNoRows.
func UpdateRole(rl *Role) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var previousName string
	if err := tx.QueryRow(`SELECT name FROM roles WHERE id = ?`, rl.ID).Scan(&previousName); err != nil {
		return err
	}
	res, err := tx.Exec(
		`UPDATE roles SET name = ?, descr = ?, brief = ?, permissions = ?, updated_at = ? WHERE id = ?`,
		rl.Name, rl.Descr, rl.Brief, permsToJSON(rl.Permissions), dbTime(time.Now()), rl.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrRoleNameTaken
		}
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	if previousName != rl.Name {
		if _, err := tx.Exec(`UPDATE group_template_agents SET role_ref = ? WHERE role_ref = ?`, rl.Name, previousName); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE spawn_profiles SET role_ref = ? WHERE role_ref = ?`, rl.Name, previousName); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE spawn_profiles
			SET role_refs = (SELECT json_group_array(CASE value WHEN ? THEN ? ELSE value END) FROM json_each(role_refs))
			WHERE EXISTS (SELECT 1 FROM json_each(role_refs) WHERE value = ?)`, previousName, rl.Name, previousName); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetRole returns the role with the given name, or (nil, nil) when no such
// role exists.
func GetRole(name string) (*Role, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rl, err := scanRole(d.QueryRow(roleSelect+` WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return rl, nil
}

// ListRoles returns every role ordered by name. Returns an empty (non-nil)
// slice when there are none.
func ListRoles() ([]*Role, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(roleSelect + ` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []*Role{}
	for rows.Next() {
		rl, err := scanRole(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rl)
	}
	return out, rows.Err()
}

// DeleteRole removes a role by name. Returns the rows affected — 0 means no
// such role, so the caller can answer 404. The wire layer refuses a delete
// while any template still references the role (see TemplatesReferencingRole);
// this DB primitive itself is unconditional, so a caller that has already
// cleared the references (or is the re-seed) can still delete.
func DeleteRole(name string) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`DELETE FROM roles WHERE name = ?`, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TemplatesReferencingRole returns the names of the group templates that have
// at least one roster agent whose role_ref points at the given role, sorted
// and de-duplicated. Empty (non-nil) when nothing references it. The delete
// guard reads this to refuse deleting a role a template still names (JOH-351),
// so the human isn't surprised by a template silently losing its role at the
// next deploy — roles resolve at DEPLOY time, so a live reference matters.
func TemplatesReferencingRole(name string) ([]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(
		`SELECT DISTINCT t.name
		   FROM group_template_agents a
		   JOIN group_templates t ON t.id = a.template_id
		  WHERE a.role_ref = ?
		  ORDER BY t.name`, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SpawnProfilesReferencingRole returns saved launch profiles whose behavioral
// role preset points at name. Role deletion is guarded against these live refs
// for the same reason template refs are guarded.
func SpawnProfilesReferencingRole(name string) ([]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT name FROM spawn_profiles
		WHERE role_ref = ? OR EXISTS (SELECT 1 FROM json_each(role_refs) WHERE value = ?)
		ORDER BY name`, name, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var profile string
		if err := rows.Scan(&profile); err != nil {
			return nil, err
		}
		out = append(out, profile)
	}
	return out, rows.Err()
}

const roleSelect = `SELECT id, name, descr, brief, permissions, created_at, updated_at FROM roles`

func scanRole(s rowScanner) (*Role, error) {
	var rl Role
	var perms string
	var createdAt, updatedAt dbTimestamp
	if err := s.Scan(&rl.ID, &rl.Name, &rl.Descr, &rl.Brief, &perms, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	rl.Permissions = permsFromJSON(perms)
	rl.CreatedAt = createdAt.Time()
	rl.UpdatedAt = updatedAt.Time()
	return &rl, nil
}
