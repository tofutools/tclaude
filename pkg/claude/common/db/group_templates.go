package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ErrGroupTemplateNameTaken is returned by CreateGroupTemplate /
// UpdateGroupTemplate when another template already owns the name.
// The name is the human-facing handle and the route key
// (/v1/templates/{name}), so it carries a UNIQUE constraint.
var ErrGroupTemplateNameTaken = errors.New("a template with that name already exists")

// GroupTemplate is a row in group_templates — a reusable blueprint for
// instantiating a working group. Unlike a group export (a conv-bound
// snapshot of a LIVE group), a template has no conv-ids: it is a recipe
// for a group that does not exist yet. Instantiating one creates a
// fresh group and spawns one new agent per Agents entry.
type GroupTemplate struct {
	ID   int64
	Name string
	// Descr is a one-line summary shown in the template list.
	Descr string
	// DefaultContext is reusable group-wide boilerplate — it becomes the
	// instantiated group's default_context (with the per-instantiation
	// task text appended), so every spawned agent sees it.
	DefaultContext string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// Agents is the ordered list of agents the template spawns, sorted
	// by Ordinal ascending.
	Agents []GroupTemplateAgent
}

// GroupTemplateAgent is a row in group_template_agents — one agent a
// template will spawn when instantiated.
type GroupTemplateAgent struct {
	ID         int64
	TemplateID int64
	// Ordinal fixes the spawn order and the display order in the editor.
	Ordinal int
	// Name is the agent's name WITHIN the template (e.g. "PO", "dev1").
	// At instantiation the final name is "<group-name>-<Name>".
	Name string
	// Role is the per-group role recorded on the membership row.
	Role string
	// Descr is the short one-line description shown on the dashboard.
	Descr string
	// InitialMessage is the agent's per-role task brief, delivered to
	// its inbox at spawn time. The per-instantiation task text is folded
	// into the group context separately, not here.
	InitialMessage string
	// IsOwner marks the agent that becomes the instantiated group's
	// owner. A template should have at most one; the instantiator grants
	// ownership to the first IsOwner agent it spawns.
	IsOwner bool
	// Permissions is the list of permission slugs granted to the agent
	// (per-conv grant overrides) right after it spawns.
	Permissions []string
}

// permsToJSON marshals a permission-slug list for the
// group_template_agents.permissions TEXT column. An empty list stores
// as "[]" so the column is never NULL and a reader can json.Unmarshal
// it unconditionally.
func permsToJSON(perms []string) string {
	if len(perms) == 0 {
		return "[]"
	}
	b, err := json.Marshal(perms)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// permsFromJSON parses the permissions TEXT column back into a slice.
// A blank or malformed value yields an empty (non-nil) slice.
func permsFromJSON(s string) []string {
	out := []string{}
	if s == "" {
		return out
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return []string{}
	}
	return out
}

// CreateGroupTemplate inserts a new template plus its agents in one
// transaction and returns the new template's ID. A name collision
// surfaces as ErrGroupTemplateNameTaken.
func CreateGroupTemplate(t *GroupTemplate) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Format(time.RFC3339Nano)
	res, err := tx.Exec(
		`INSERT INTO group_templates (name, descr, default_context, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		t.Name, t.Descr, t.DefaultContext, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrGroupTemplateNameTaken
		}
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := insertTemplateAgents(tx, id, t.Agents); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// UpdateGroupTemplate rewrites an existing template (identified by
// t.ID) and replaces its agent list wholesale — the editor always
// posts the full desired state, so a delete-then-reinsert keeps the
// stored rows in lockstep with the form. Renaming to a name another
// template holds surfaces as ErrGroupTemplateNameTaken.
func UpdateGroupTemplate(t *GroupTemplate) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(
		`UPDATE group_templates SET name = ?, descr = ?, default_context = ?, updated_at = ?
		 WHERE id = ?`,
		t.Name, t.Descr, t.DefaultContext, time.Now().Format(time.RFC3339Nano), t.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrGroupTemplateNameTaken
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
	if _, err := tx.Exec(`DELETE FROM group_template_agents WHERE template_id = ?`, t.ID); err != nil {
		return err
	}
	if err := insertTemplateAgents(tx, t.ID, t.Agents); err != nil {
		return err
	}
	return tx.Commit()
}

// insertTemplateAgents writes the agent rows for a template. The
// caller's Ordinal values are honoured verbatim so the editor's row
// order round-trips.
func insertTemplateAgents(tx *sql.Tx, templateID int64, agents []GroupTemplateAgent) error {
	for _, a := range agents {
		owner := 0
		if a.IsOwner {
			owner = 1
		}
		if _, err := tx.Exec(
			`INSERT INTO group_template_agents
			   (template_id, ordinal, name, role, descr, initial_message, is_owner, permissions)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			templateID, a.Ordinal, a.Name, a.Role, a.Descr, a.InitialMessage,
			owner, permsToJSON(a.Permissions)); err != nil {
			return err
		}
	}
	return nil
}

// GetGroupTemplate returns the template with the given name, agents
// included. Returns (nil, nil) when no such template exists.
func GetGroupTemplate(name string) (*GroupTemplate, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	t, err := scanGroupTemplate(d.QueryRow(
		`SELECT id, name, descr, default_context, created_at, updated_at
		 FROM group_templates WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if t.Agents, err = listTemplateAgents(d, t.ID); err != nil {
		return nil, err
	}
	return t, nil
}

// ListGroupTemplates returns every template, agents included, ordered
// by name. Returns an empty (non-nil) slice when there are none.
func ListGroupTemplates() ([]*GroupTemplate, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(
		`SELECT id, name, descr, default_context, created_at, updated_at
		 FROM group_templates ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []*GroupTemplate{}
	byID := map[int64]*GroupTemplate{}
	for rows.Next() {
		t, err := scanGroupTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
		byID[t.ID] = t
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// One pass over every agent row, bucketed onto its template — avoids
	// an N+1 query when the editor list is rendered.
	agentRows, err := d.Query(
		`SELECT id, template_id, ordinal, name, role, descr, initial_message, is_owner, permissions
		 FROM group_template_agents ORDER BY template_id, ordinal, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = agentRows.Close() }()
	for agentRows.Next() {
		a, err := scanGroupTemplateAgent(agentRows)
		if err != nil {
			return nil, err
		}
		if t := byID[a.TemplateID]; t != nil {
			t.Agents = append(t.Agents, *a)
		}
	}
	if err := agentRows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteGroupTemplate removes a template by name. The agent rows go
// with it via ON DELETE CASCADE. Returns the rows affected — 0 means
// no such template, so the caller can answer 404.
func DeleteGroupTemplate(name string) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`DELETE FROM group_templates WHERE name = ?`, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// listTemplateAgents loads one template's agent rows, ordered.
func listTemplateAgents(d *sql.DB, templateID int64) ([]GroupTemplateAgent, error) {
	rows, err := d.Query(
		`SELECT id, template_id, ordinal, name, role, descr, initial_message, is_owner, permissions
		 FROM group_template_agents WHERE template_id = ? ORDER BY ordinal, id`, templateID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []GroupTemplateAgent{}
	for rows.Next() {
		a, err := scanGroupTemplateAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func scanGroupTemplate(s rowScanner) (*GroupTemplate, error) {
	var t GroupTemplate
	var createdAt, updatedAt string
	if err := s.Scan(&t.ID, &t.Name, &t.Descr, &t.DefaultContext, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	t.CreatedAt = parseTimeOrZero(createdAt)
	t.UpdatedAt = parseTimeOrZero(updatedAt)
	t.Agents = []GroupTemplateAgent{}
	return &t, nil
}

func scanGroupTemplateAgent(s rowScanner) (*GroupTemplateAgent, error) {
	var a GroupTemplateAgent
	var owner int
	var perms string
	if err := s.Scan(&a.ID, &a.TemplateID, &a.Ordinal, &a.Name, &a.Role,
		&a.Descr, &a.InitialMessage, &owner, &perms); err != nil {
		return nil, err
	}
	a.IsOwner = owner != 0
	a.Permissions = permsFromJSON(perms)
	return &a, nil
}

// isUniqueViolation reports whether err is a SQLite UNIQUE-constraint
// failure. modernc.org/sqlite surfaces these as a plain error whose
// message contains "UNIQUE constraint failed"; there is no typed
// sentinel to errors.Is against, so a substring match is the pragmatic
// check.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
