package db

import (
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
)

// ErrSpawnProfileNameTaken is returned by CreateSpawnProfile /
// UpdateSpawnProfile when another profile already owns the name. The name is
// the human-facing handle and the route key (/v1/spawn-profiles/{name}), so it
// carries a UNIQUE constraint.
var ErrSpawnProfileNameTaken = errors.New("a spawn profile with that name or alias already exists")

// SpawnProfile is a row in spawn_profiles — a named, reusable bundle of the
// dashboard's spawn-agent dialog (JOH-210). Pressing Spawn in a group with a
// default profile pre-fills the dialog from it; the daemon also resolves a
// group's default profile server-side to fill blank LAUNCH fields for
// non-dialog spawns (group templates).
//
// Every field is OPTIONAL. Text fields use "" for unset (loads blank). The
// five toggles are *bool so the model can distinguish unset (nil → leave the
// dialog's own default) from an explicit off (false) or on (true); they map to
// the NULLABLE INTEGER columns. cwd / worktree are deliberately NOT stored —
// they are per-spawn, not reusable.
type SpawnProfile struct {
	ID   int64
	Name string // the profile handle (UNIQUE)
	// Aliases are alternate handles for this same row. They share one namespace
	// with every primary profile name and alias.
	Aliases []string
	// DisabledReason is empty for an enabled profile. A non-empty reason keeps
	// the profile visible and referenced but makes every spawn path that would
	// use it fail loudly instead of silently falling through.
	DisabledReason string

	// Launch fields — overlap clcommon.SpawnArgs. "" = unset.
	Harness  string
	Model    string
	Effort   string
	Sandbox  string
	Approval string
	// AskUserQuestionTimeout is the profile's Claude Code AskUserQuestion
	// idle-timeout default (never|60s|5m|10m), delivered per-spawn via
	// `--settings`; "" = unset (the agent uses the operator's settings.json). A
	// Claude-Code-only launch field, validated against the profile's harness.
	AskUserQuestionTimeout string
	// AutoReview / TrustDir are launch toggles; nil = unset.
	AutoReview *bool
	TrustDir   *bool
	// RemoteControl is the profile's "start with Claude Code Remote Access on"
	// default — tri-state: nil = unset, false = off, true = on. Resolved at
	// spawn under a group's remote-control policy, which overrides it (JOH-262).
	RemoteControl *bool

	// Identity / enrollment fields (dialog-side). "" = unset.
	AgentName      string // the dialog's "Name" field (the spawned agent's display name)
	Role           string
	Descr          string
	InitialMessage string

	// Dialog toggles; nil = unset.
	SyncWorktree               *bool
	AutoFocus                  *bool
	IncludeGroupDefaultContext *bool

	// Birth-time access controls the spawn dialog can pre-fill from a profile
	//. IsOwner is tri-state (nil = unset → leave the dialog's own
	// default): when set it pre-checks "Group owner". PermissionOverrides is the
	// saved per-slug override map (slug → "grant" | "deny"), stored as a JSON
	// object in the permission_overrides column ("" = none) and pre-loaded into
	// the dialog's buffered editor.
	IsOwner             *bool
	PermissionOverrides map[string]string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// boolPtrToNull maps a tri-state *bool to a nullable INTEGER column: nil →
// NULL (unset), false → 0, true → 1.
func boolPtrToNull(b *bool) sql.NullInt64 {
	if b == nil {
		return sql.NullInt64{}
	}
	v := int64(0)
	if *b {
		v = 1
	}
	return sql.NullInt64{Int64: v, Valid: true}
}

// nullToBoolPtr maps a nullable INTEGER column back to a tri-state *bool: NULL
// → nil (unset), 0 → false, non-zero → true.
func nullToBoolPtr(n sql.NullInt64) *bool {
	if !n.Valid {
		return nil
	}
	b := n.Int64 != 0
	return &b
}

// nullToInt64Ptr maps a nullable INTEGER column back to a *int64: NULL → nil,
// otherwise a pointer to the value. Used for optional foreign keys like
// agent_groups.parent_id (NULL = top-level, no parent).
func nullToInt64Ptr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// CreateSpawnProfile inserts a new profile and returns its ID. A name
// collision surfaces as ErrSpawnProfileNameTaken.
func CreateSpawnProfile(p *SpawnProfile) (int64, error) {
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
		`INSERT INTO spawn_profiles
		   (name, disabled_reason, harness, model, effort, sandbox, approval, ask_user_question_timeout,
		    auto_review, trust_dir,
		    agent_name, role, descr, initial_message,
		    sync_worktree, auto_focus, include_group_default_context, remote_control,
		    is_owner, permission_overrides,
		    created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.DisabledReason, p.Harness, p.Model, p.Effort, p.Sandbox, p.Approval, p.AskUserQuestionTimeout,
		boolPtrToNull(p.AutoReview), boolPtrToNull(p.TrustDir),
		p.AgentName, p.Role, p.Descr, p.InitialMessage,
		boolPtrToNull(p.SyncWorktree), boolPtrToNull(p.AutoFocus),
		boolPtrToNull(p.IncludeGroupDefaultContext), boolPtrToNull(p.RemoteControl),
		boolPtrToNull(p.IsOwner), marshalPermissionOverrides(p.PermissionOverrides),
		now, now)
	if err != nil {
		if isSpawnProfileHandleViolation(err) {
			return 0, ErrSpawnProfileNameTaken
		}
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := replaceSpawnProfileAliases(tx, id, p.Aliases); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// UpdateSpawnProfile rewrites an existing profile identified by p.ID. Renaming
// to a name another profile holds surfaces as ErrSpawnProfileNameTaken; a
// missing ID returns sql.ErrNoRows.
func UpdateSpawnProfile(p *SpawnProfile) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Remove this profile's existing aliases before changing its primary name.
	// This permits an atomic promotion such as `primary` + alias `reviewer` →
	// primary `reviewer` + alias `primary`, while aliases owned by other rows
	// remain protected by the database triggers.
	if _, err := tx.Exec(`DELETE FROM spawn_profile_aliases WHERE profile_id = ?`, p.ID); err != nil {
		return err
	}
	res, err := tx.Exec(
		`UPDATE spawn_profiles SET
		   name = ?, disabled_reason = ?, harness = ?, model = ?, effort = ?, sandbox = ?, approval = ?,
		   ask_user_question_timeout = ?,
		   auto_review = ?, trust_dir = ?,
		   agent_name = ?, role = ?, descr = ?, initial_message = ?,
		   sync_worktree = ?, auto_focus = ?, include_group_default_context = ?, remote_control = ?,
		   is_owner = ?, permission_overrides = ?,
		   updated_at = ?
		 WHERE id = ?`,
		p.Name, p.DisabledReason, p.Harness, p.Model, p.Effort, p.Sandbox, p.Approval,
		p.AskUserQuestionTimeout,
		boolPtrToNull(p.AutoReview), boolPtrToNull(p.TrustDir),
		p.AgentName, p.Role, p.Descr, p.InitialMessage,
		boolPtrToNull(p.SyncWorktree), boolPtrToNull(p.AutoFocus),
		boolPtrToNull(p.IncludeGroupDefaultContext), boolPtrToNull(p.RemoteControl),
		boolPtrToNull(p.IsOwner), marshalPermissionOverrides(p.PermissionOverrides),
		time.Now().Format(time.RFC3339Nano), p.ID)
	if err != nil {
		if isSpawnProfileHandleViolation(err) {
			return ErrSpawnProfileNameTaken
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
	if err := replaceSpawnProfileAliases(tx, p.ID, p.Aliases); err != nil {
		return err
	}
	// Names are API/export snapshots; the durable *_id columns are the actual
	// references. Refresh every snapshot in the same transaction so all UIs
	// immediately present the profile's current name after a rename.
	for _, stmt := range []string{
		`UPDATE agent_groups SET default_profile = ? WHERE default_profile_id = ?`,
		`UPDATE group_template_agents SET spawn_profile = ? WHERE spawn_profile_id = ?`,
		`UPDATE roles SET spawn_profile = ? WHERE spawn_profile_id = ?`,
	} {
		if _, err := tx.Exec(stmt, p.Name, p.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE dashboard_prefs SET value = ?, updated_at = ?
		WHERE key = 'tclaude.dash.default_profile'
		  AND EXISTS (SELECT 1 FROM dashboard_prefs ids
		               WHERE ids.key = 'tclaude.dash.default_profile_id' AND ids.value = CAST(? AS TEXT))`,
		p.Name, time.Now().Format(time.RFC3339Nano), p.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// GetSpawnProfile returns the profile with the given name, or (nil, nil) when
// no such profile exists.
func GetSpawnProfile(name string) (*SpawnProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	p, err := scanSpawnProfile(d.QueryRow(spawnProfileSelect+` WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := loadSpawnProfileAliases(d, p); err != nil {
		return nil, err
	}
	return p, nil
}

// ResolveSpawnProfile resolves either a primary name or one of its aliases.
// GetSpawnProfile intentionally remains primary-name-only for management and
// import code that must distinguish an alias collision from an exact row.
func ResolveSpawnProfile(handle string) (*SpawnProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var id int64
	err = d.QueryRow(`SELECT p.id
		FROM spawn_profiles p
		LEFT JOIN spawn_profile_aliases a ON a.profile_id = p.id
		WHERE p.name = ? OR a.alias = ?
		LIMIT 1`, handle, handle).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return getSpawnProfileByIDDB(d, id)
}

// GetSpawnProfileByID returns the profile with the stable row id. Registry
// references use this path so renaming the human-facing handle cannot detach
// them.
func GetSpawnProfileByID(id int64) (*SpawnProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return getSpawnProfileByIDDB(d, id)
}

func getSpawnProfileByIDDB(d *sql.DB, id int64) (*SpawnProfile, error) {
	p, err := scanSpawnProfile(d.QueryRow(spawnProfileSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := loadSpawnProfileAliases(d, p); err != nil {
		return nil, err
	}
	return p, nil
}

// ListSpawnProfiles returns every profile ordered by name. Returns an empty
// (non-nil) slice when there are none.
func ListSpawnProfiles() ([]*SpawnProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(spawnProfileSelect + ` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []*SpawnProfile{}
	for rows.Next() {
		p, err := scanSpawnProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, p := range out {
		if err := loadSpawnProfileAliases(d, p); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteSpawnProfile removes a profile by name. Returns the rows affected — 0
// means no such profile, so the caller can answer 404.
func DeleteSpawnProfile(name string) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	if err := tx.QueryRow(`SELECT id FROM spawn_profiles WHERE name = ?`, name).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	// Clear resolved references before deleting. This prevents a later profile
	// that reuses the same name from silently inheriting the old links.
	for _, stmt := range []string{
		`UPDATE agent_groups SET default_profile = '', default_profile_id = NULL WHERE default_profile_id = ?`,
		`UPDATE group_template_agents SET spawn_profile = '', spawn_profile_id = NULL WHERE spawn_profile_id = ?`,
		`UPDATE roles SET spawn_profile = '', spawn_profile_id = NULL WHERE spawn_profile_id = ?`,
	} {
		if _, err := tx.Exec(stmt, id); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(`DELETE FROM dashboard_prefs
		WHERE key IN ('tclaude.dash.default_profile', 'tclaude.dash.default_profile_id')
		  AND EXISTS (SELECT 1 FROM dashboard_prefs ids
		               WHERE ids.key = 'tclaude.dash.default_profile_id' AND ids.value = CAST(? AS TEXT))`, id); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM spawn_profiles WHERE id = ?`, id)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

type spawnProfileAliasTx interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func replaceSpawnProfileAliases(tx spawnProfileAliasTx, profileID int64, aliases []string) error {
	if _, err := tx.Exec(`DELETE FROM spawn_profile_aliases WHERE profile_id = ?`, profileID); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" || seen[alias] {
			continue
		}
		seen[alias] = true
		if _, err := tx.Exec(`INSERT INTO spawn_profile_aliases (alias, profile_id) VALUES (?, ?)`, alias, profileID); err != nil {
			if isSpawnProfileHandleViolation(err) {
				return ErrSpawnProfileNameTaken
			}
			return err
		}
	}
	return nil
}

func loadSpawnProfileAliases(d *sql.DB, p *SpawnProfile) error {
	rows, err := d.Query(`SELECT alias FROM spawn_profile_aliases WHERE profile_id = ? ORDER BY alias`, p.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	p.Aliases = []string{}
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			return err
		}
		p.Aliases = append(p.Aliases, alias)
	}
	sort.Strings(p.Aliases)
	return rows.Err()
}

func isSpawnProfileHandleViolation(err error) bool {
	return isUniqueViolation(err) || (err != nil && strings.Contains(err.Error(), "spawn profile handle already exists"))
}

const spawnProfileSelect = `SELECT id, name, disabled_reason, harness, model, effort, sandbox, approval,
	ask_user_question_timeout,
	auto_review, trust_dir, agent_name, role, descr, initial_message,
	sync_worktree, auto_focus, include_group_default_context, remote_control,
	is_owner, permission_overrides, created_at, updated_at
	FROM spawn_profiles`

func scanSpawnProfile(s rowScanner) (*SpawnProfile, error) {
	var p SpawnProfile
	var autoReview, trustDir, syncWorktree, autoFocus, includeCtx, remoteControl, isOwner sql.NullInt64
	var permOverrides, createdAt, updatedAt string
	if err := s.Scan(&p.ID, &p.Name, &p.DisabledReason, &p.Harness, &p.Model, &p.Effort, &p.Sandbox, &p.Approval,
		&p.AskUserQuestionTimeout,
		&autoReview, &trustDir, &p.AgentName, &p.Role, &p.Descr, &p.InitialMessage,
		&syncWorktree, &autoFocus, &includeCtx, &remoteControl,
		&isOwner, &permOverrides, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	p.AutoReview = nullToBoolPtr(autoReview)
	p.TrustDir = nullToBoolPtr(trustDir)
	p.SyncWorktree = nullToBoolPtr(syncWorktree)
	p.AutoFocus = nullToBoolPtr(autoFocus)
	p.IncludeGroupDefaultContext = nullToBoolPtr(includeCtx)
	p.RemoteControl = nullToBoolPtr(remoteControl)
	p.IsOwner = nullToBoolPtr(isOwner)
	p.PermissionOverrides = unmarshalPermissionOverrides(permOverrides)
	p.CreatedAt = parseTimeOrZero(createdAt)
	p.UpdatedAt = parseTimeOrZero(updatedAt)
	return &p, nil
}
