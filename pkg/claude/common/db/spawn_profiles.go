package db

import (
	"database/sql"
	"errors"
	"time"
)

// ErrSpawnProfileNameTaken is returned by CreateSpawnProfile /
// UpdateSpawnProfile when another profile already owns the name. The name is
// the human-facing handle and the route key (/v1/spawn-profiles/{name}), so it
// carries a UNIQUE constraint.
var ErrSpawnProfileNameTaken = errors.New("a spawn profile with that name already exists")

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

	// Launch fields — overlap clcommon.SpawnArgs. "" = unset.
	Harness  string
	Model    string
	Effort   string
	Sandbox  string
	Approval string
	// AutoReview / TrustDir are launch toggles; nil = unset.
	AutoReview *bool
	TrustDir   *bool

	// Identity / enrollment fields (dialog-side). "" = unset.
	AgentName      string // the dialog's "Name" field (the spawned agent's display name)
	Role           string
	Descr          string
	InitialMessage string

	// Dialog toggles; nil = unset.
	SyncWorktree               *bool
	AutoFocus                  *bool
	IncludeGroupDefaultContext *bool

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

// CreateSpawnProfile inserts a new profile and returns its ID. A name
// collision surfaces as ErrSpawnProfileNameTaken.
func CreateSpawnProfile(p *SpawnProfile) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	now := time.Now().Format(time.RFC3339Nano)
	res, err := d.Exec(
		`INSERT INTO spawn_profiles
		   (name, harness, model, effort, sandbox, approval, auto_review, trust_dir,
		    agent_name, role, descr, initial_message,
		    sync_worktree, auto_focus, include_group_default_context,
		    created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.Harness, p.Model, p.Effort, p.Sandbox, p.Approval,
		boolPtrToNull(p.AutoReview), boolPtrToNull(p.TrustDir),
		p.AgentName, p.Role, p.Descr, p.InitialMessage,
		boolPtrToNull(p.SyncWorktree), boolPtrToNull(p.AutoFocus),
		boolPtrToNull(p.IncludeGroupDefaultContext),
		now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrSpawnProfileNameTaken
		}
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateSpawnProfile rewrites an existing profile identified by p.ID. Renaming
// to a name another profile holds surfaces as ErrSpawnProfileNameTaken; a
// missing ID returns sql.ErrNoRows.
func UpdateSpawnProfile(p *SpawnProfile) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(
		`UPDATE spawn_profiles SET
		   name = ?, harness = ?, model = ?, effort = ?, sandbox = ?, approval = ?,
		   auto_review = ?, trust_dir = ?,
		   agent_name = ?, role = ?, descr = ?, initial_message = ?,
		   sync_worktree = ?, auto_focus = ?, include_group_default_context = ?,
		   updated_at = ?
		 WHERE id = ?`,
		p.Name, p.Harness, p.Model, p.Effort, p.Sandbox, p.Approval,
		boolPtrToNull(p.AutoReview), boolPtrToNull(p.TrustDir),
		p.AgentName, p.Role, p.Descr, p.InitialMessage,
		boolPtrToNull(p.SyncWorktree), boolPtrToNull(p.AutoFocus),
		boolPtrToNull(p.IncludeGroupDefaultContext),
		time.Now().Format(time.RFC3339Nano), p.ID)
	if err != nil {
		if isUniqueViolation(err) {
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
	return nil
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
	return out, rows.Err()
}

// DeleteSpawnProfile removes a profile by name. Returns the rows affected — 0
// means no such profile, so the caller can answer 404.
func DeleteSpawnProfile(name string) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`DELETE FROM spawn_profiles WHERE name = ?`, name)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

const spawnProfileSelect = `SELECT id, name, harness, model, effort, sandbox, approval,
	auto_review, trust_dir, agent_name, role, descr, initial_message,
	sync_worktree, auto_focus, include_group_default_context, created_at, updated_at
	FROM spawn_profiles`

func scanSpawnProfile(s rowScanner) (*SpawnProfile, error) {
	var p SpawnProfile
	var autoReview, trustDir, syncWorktree, autoFocus, includeCtx sql.NullInt64
	var createdAt, updatedAt string
	if err := s.Scan(&p.ID, &p.Name, &p.Harness, &p.Model, &p.Effort, &p.Sandbox, &p.Approval,
		&autoReview, &trustDir, &p.AgentName, &p.Role, &p.Descr, &p.InitialMessage,
		&syncWorktree, &autoFocus, &includeCtx, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	p.AutoReview = nullToBoolPtr(autoReview)
	p.TrustDir = nullToBoolPtr(trustDir)
	p.SyncWorktree = nullToBoolPtr(syncWorktree)
	p.AutoFocus = nullToBoolPtr(autoFocus)
	p.IncludeGroupDefaultContext = nullToBoolPtr(includeCtx)
	p.CreatedAt = parseTimeOrZero(createdAt)
	p.UpdatedAt = parseTimeOrZero(updatedAt)
	return &p, nil
}
