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
	// WorkPattern is the template's default work pattern (JOH-336): an
	// ORDERED list of routed briefing messages delivered — in order —
	// after the whole roster has spawned. Each entry goes to one roster
	// agent by name, or to every member ("all"). Distinct from a per-agent
	// InitialMessage (that is the agent's own role brief, delivered at its
	// spawn): the pattern is the cross-cutting choreography layer —
	// "brief the Lead with the leadership frame, then everyone with the
	// house rules". Empty = no pattern (today's behaviour).
	WorkPattern []WorkPatternEntry
	// Process is the template's declarative process spec (JOH-242): an
	// ORDERED list of phases, each a {name, roles, criteria} chapter of the
	// group's work. It is ADVISORY — rendered into briefings and tracked at
	// runtime, but never enforced (no gates, no phase-scoped permissions). A
	// group instantiated from the template snapshots this into its runtime
	// process state. Empty = no process (the feature is off for the group).
	Process []ProcessPhase
	// Rhythms is the template's recurring-nudge declarations (JOH-244): the
	// party's "drumbeats". At deploy each is materialized as a normal cron job
	// targeting the instantiated group, role-filtered at fire time. Empty = no
	// rhythms (today's behaviour).
	Rhythms []Rhythm
	// WaveMaxWait caps (in seconds) how long a staged-spawn wave gate waits for
	// the prior wave to go idle before the next wave spawns anyway (JOH-244). 0
	// = use the built-in default. A crashed lead can't wedge the force forever.
	WaveMaxWait int
}

// WorkPatternEntry is one routed briefing message in a template's work
// pattern. SendTo is a roster agent's template-name or the literal
// "all". Value supports the {{task}} placeholder, replaced with the
// per-instantiation task text at delivery.
type WorkPatternEntry struct {
	SendTo string `json:"send_to"`
	Value  string `json:"value"`
}

// workPatternToJSON marshals a work pattern for the
// group_templates.work_pattern TEXT column. An empty pattern stores as
// "[]" (the permsToJSON convention) so a reader can json.Unmarshal it
// unconditionally; legacy rows hold ” and read back as empty.
func workPatternToJSON(entries []WorkPatternEntry) string {
	if len(entries) == 0 {
		return "[]"
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// workPatternFromJSON parses the work_pattern TEXT column back into a
// slice. A blank (” — pre-v87 rows) or malformed value yields an empty
// (non-nil) slice.
func workPatternFromJSON(s string) []WorkPatternEntry {
	out := []WorkPatternEntry{}
	if s == "" {
		return out
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return []WorkPatternEntry{}
	}
	return out
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
	// IsOwner marks an agent that becomes an owner of the instantiated
	// group. A group can have several owners, so a template may mark
	// several; each IsOwner agent is granted ownership after it spawns.
	IsOwner bool
	// Permissions is the list of permission slugs granted to the agent
	// (per-conv grant overrides) right after it spawns.
	Permissions []string

	// RoleRef is a by-name reference to a roles row (JOH-240): the agent
	// inherits that role's defaults (canonical role-brief, launch shape,
	// permission set) BENEATH its own overrides. No DB-level FK — existence is
	// validated at the wire boundary, following SpawnProfile. "" = no role.
	RoleRef string

	// Per-role launch profile (JOH-239). SpawnProfile is a by-name reference
	// to a spawn_profiles row (no DB-level FK — existence is validated at the
	// wire boundary, following resolveGroupDefaultProfileName). The five inline
	// fields are per-agent launch overrides that win over the referenced
	// profile. All "" = unset: the resolver falls through to the referenced
	// profile, then the group default profile, then the harness default. At
	// instantiate the effective launch shape is
	//   per-agent inline override → referenced profile → group default → harness default.
	SpawnProfile string
	Harness      string
	Model        string
	Effort       string
	Sandbox      string
	Approval     string

	// ProfileInline is the agent's template-LOCAL spawn profile (nil = none),
	// stored as a JSON object in the profile_inline column. It carries the same
	// launch + birth-time-access shape as a spawn_profiles row but lives inside
	// the template: a bespoke per-agent launch config that doesn't pollute the
	// shared profile registry and travels with the template on export/import.
	// Only the fields the template deploy path honours are persisted (launch
	// fields, ask-timeout, trust_dir/auto_review/remote_control toggles, owner +
	// permission overrides) — see inlineProfileToJSON. Resolution order at
	// instantiate: the five legacy inline fields above → ProfileInline →
	// SpawnProfile (the registry reference) → role tiers → harness default.
	ProfileInline *SpawnProfile

	// Wave is the agent's staged-spawn wave (JOH-244), default 0. Waves spawn
	// in ascending order: wave N+1 starts only once wave N's agents are up and
	// have gone idle (or a per-template max-wait cap fires). A template whose
	// every agent is wave 0 spawns in a single synchronous pass — today's exact
	// behaviour.
	Wave int
}

// templateInlineProfileJSON is the serialized shape of a template-local spawn
// profile (the group_template_agents.profile_inline column). snake_case field
// names deliberately mirror the daemon's spawn-profile wire shape so the
// column, the template wire JSON and a template export envelope all agree.
// Restricted to the fields the template deploy path honours — identity fields
// (agent_name/role/descr/initial_message) live on the template-agent row
// itself, and the spawn-dialog-only toggles (sync_worktree/auto_focus/
// include_group_default_context) have no meaning for a template deploy.
type templateInlineProfileJSON struct {
	Harness                string            `json:"harness,omitempty"`
	Model                  string            `json:"model,omitempty"`
	Effort                 string            `json:"effort,omitempty"`
	Sandbox                string            `json:"sandbox,omitempty"`
	Approval               string            `json:"approval,omitempty"`
	AskUserQuestionTimeout string            `json:"ask_user_question_timeout,omitempty"`
	AutoReview             *bool             `json:"auto_review,omitempty"`
	TrustDir               *bool             `json:"trust_dir,omitempty"`
	RemoteControl          *bool             `json:"remote_control,omitempty"`
	IsOwner                *bool             `json:"is_owner,omitempty"`
	PermissionOverrides    map[string]string `json:"permission_overrides,omitempty"`
}

// inlineProfileToJSON marshals a template-local profile for the
// profile_inline TEXT column. nil stores as "" (no inline profile), the
// permsToJSON convention for "absent".
func inlineProfileToJSON(p *SpawnProfile) string {
	if p == nil {
		return ""
	}
	b, err := json.Marshal(templateInlineProfileJSON{
		Harness:                p.Harness,
		Model:                  p.Model,
		Effort:                 p.Effort,
		Sandbox:                p.Sandbox,
		Approval:               p.Approval,
		AskUserQuestionTimeout: p.AskUserQuestionTimeout,
		AutoReview:             p.AutoReview,
		TrustDir:               p.TrustDir,
		RemoteControl:          p.RemoteControl,
		IsOwner:                p.IsOwner,
		PermissionOverrides:    p.PermissionOverrides,
	})
	if err != nil {
		return ""
	}
	return string(b)
}

// inlineProfileFromJSON parses the profile_inline column back into a
// *SpawnProfile (nil for blank/malformed — no inline profile). The returned
// profile has no Name/ID: it is template-local by definition.
func inlineProfileFromJSON(s string) *SpawnProfile {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var j templateInlineProfileJSON
	if err := json.Unmarshal([]byte(s), &j); err != nil {
		return nil
	}
	return &SpawnProfile{
		Harness:                j.Harness,
		Model:                  j.Model,
		Effort:                 j.Effort,
		Sandbox:                j.Sandbox,
		Approval:               j.Approval,
		AskUserQuestionTimeout: j.AskUserQuestionTimeout,
		AutoReview:             j.AutoReview,
		TrustDir:               j.TrustDir,
		RemoteControl:          j.RemoteControl,
		IsOwner:                j.IsOwner,
		PermissionOverrides:    j.PermissionOverrides,
	}
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
		`INSERT INTO group_templates (name, descr, default_context, work_pattern, process, rhythms, wave_max_wait, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Name, t.Descr, t.DefaultContext, workPatternToJSON(t.WorkPattern), processToJSON(t.Process),
		rhythmsToJSON(t.Rhythms), t.WaveMaxWait, now, now)
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
		`UPDATE group_templates SET name = ?, descr = ?, default_context = ?, work_pattern = ?, process = ?, rhythms = ?, wave_max_wait = ?, updated_at = ?
		 WHERE id = ?`,
		t.Name, t.Descr, t.DefaultContext, workPatternToJSON(t.WorkPattern), processToJSON(t.Process),
		rhythmsToJSON(t.Rhythms), t.WaveMaxWait, time.Now().Format(time.RFC3339Nano), t.ID)
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
			   (template_id, ordinal, name, role, descr, initial_message, is_owner, permissions,
			    role_ref, spawn_profile, harness, model, effort, sandbox, approval, wave, profile_inline)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			templateID, a.Ordinal, a.Name, a.Role, a.Descr, a.InitialMessage,
			owner, permsToJSON(a.Permissions),
			a.RoleRef, a.SpawnProfile, a.Harness, a.Model, a.Effort, a.Sandbox, a.Approval, a.Wave,
			inlineProfileToJSON(a.ProfileInline)); err != nil {
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
		`SELECT id, name, descr, default_context, work_pattern, process, rhythms, wave_max_wait, created_at, updated_at
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
		`SELECT id, name, descr, default_context, work_pattern, process, rhythms, wave_max_wait, created_at, updated_at
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
		`SELECT id, template_id, ordinal, name, role, descr, initial_message, is_owner, permissions,
		        role_ref, spawn_profile, harness, model, effort, sandbox, approval, wave, profile_inline
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
		`SELECT id, template_id, ordinal, name, role, descr, initial_message, is_owner, permissions,
		        role_ref, spawn_profile, harness, model, effort, sandbox, approval, wave, profile_inline
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
	var createdAt, updatedAt, workPattern, process, rhythms string
	if err := s.Scan(&t.ID, &t.Name, &t.Descr, &t.DefaultContext, &workPattern, &process, &rhythms, &t.WaveMaxWait, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	t.CreatedAt = parseTimeOrZero(createdAt)
	t.UpdatedAt = parseTimeOrZero(updatedAt)
	t.Agents = []GroupTemplateAgent{}
	t.WorkPattern = workPatternFromJSON(workPattern)
	t.Process = processFromJSON(process)
	t.Rhythms = rhythmsFromJSON(rhythms)
	return &t, nil
}

func scanGroupTemplateAgent(s rowScanner) (*GroupTemplateAgent, error) {
	var a GroupTemplateAgent
	var owner int
	var perms, profileInline string
	if err := s.Scan(&a.ID, &a.TemplateID, &a.Ordinal, &a.Name, &a.Role,
		&a.Descr, &a.InitialMessage, &owner, &perms,
		&a.RoleRef, &a.SpawnProfile, &a.Harness, &a.Model, &a.Effort, &a.Sandbox, &a.Approval, &a.Wave,
		&profileInline); err != nil {
		return nil, err
	}
	a.IsOwner = owner != 0
	a.Permissions = permsFromJSON(perms)
	a.ProfileInline = inlineProfileFromJSON(profileInline)
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
