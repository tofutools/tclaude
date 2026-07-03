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
// briefing), a default launch shape (the same six fields template agents got
// in v89), and a default permission set.
//
// A template agent references a role by name (group_template_agents.role_ref);
// the referenced role's defaults sit BENEATH the agent's own overrides at
// instantiate: agent inline → agent profile → role → harness default (the
// group-default tier is empty for a freshly-instantiated group). All launch
// text fields use "" for unset ("inherit"), mirroring spawn_profiles and the
// per-agent launch fields.
type Role struct {
	ID   int64
	Name string // the role handle (UNIQUE)
	// Descr is a one-line summary shown in the role list.
	Descr string
	// Brief is the canonical role-brief — guidance rendered into a "## Role"
	// block in the composed startup context of any agent referencing this
	// role. "" = no brief (the block is omitted).
	Brief string

	// Default launch shape — the same six fields template agents carry
	// (JOH-239). SpawnProfile is a by-name reference to a spawn_profiles row;
	// the five inline fields are inline defaults. "" = unset (inherit).
	SpawnProfile string
	Harness      string
	Model        string
	Effort       string
	Sandbox      string
	Approval     string

	// Permissions is the role's default permission-slug set, merged beneath a
	// referencing agent's own permission grants at instantiate (union, agent
	// extends, deduped). Stored as a JSON list like group_template_agents.
	Permissions []string

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
	now := time.Now().Format(time.RFC3339Nano)
	res, err := d.Exec(
		`INSERT INTO roles
		   (name, descr, brief, spawn_profile, harness, model, effort, sandbox, approval,
		    permissions, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rl.Name, rl.Descr, rl.Brief, rl.SpawnProfile, rl.Harness, rl.Model, rl.Effort,
		rl.Sandbox, rl.Approval, permsToJSON(rl.Permissions), now, now)
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
	res, err := d.Exec(
		`UPDATE roles SET
		   name = ?, descr = ?, brief = ?, spawn_profile = ?, harness = ?, model = ?,
		   effort = ?, sandbox = ?, approval = ?, permissions = ?, updated_at = ?
		 WHERE id = ?`,
		rl.Name, rl.Descr, rl.Brief, rl.SpawnProfile, rl.Harness, rl.Model, rl.Effort,
		rl.Sandbox, rl.Approval, permsToJSON(rl.Permissions),
		time.Now().Format(time.RFC3339Nano), rl.ID)
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
	return nil
}

// GetRole returns the role with the given name, or (nil, nil) when no such
// role exists.
func GetRole(name string) (*Role, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rl, err := scanRole(d.QueryRow(roleSelect + ` WHERE name = ?`, name))
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
// such role, so the caller can answer 404. A template agent referencing a
// now-deleted role degrades gracefully (its role_ref resolves to nothing and
// the agent falls through to its own overrides / harness defaults).
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

const roleSelect = `SELECT id, name, descr, brief, spawn_profile, harness, model, effort,
	sandbox, approval, permissions, created_at, updated_at
	FROM roles`

func scanRole(s rowScanner) (*Role, error) {
	var rl Role
	var perms, createdAt, updatedAt string
	if err := s.Scan(&rl.ID, &rl.Name, &rl.Descr, &rl.Brief, &rl.SpawnProfile, &rl.Harness,
		&rl.Model, &rl.Effort, &rl.Sandbox, &rl.Approval, &perms, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	rl.Permissions = permsFromJSON(perms)
	rl.CreatedAt = parseTimeOrZero(createdAt)
	rl.UpdatedAt = parseTimeOrZero(updatedAt)
	return &rl, nil
}
