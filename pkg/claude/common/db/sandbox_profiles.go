package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

var (
	ErrSandboxProfileNameTaken     = errors.New("a sandbox profile with that name already exists")
	ErrSandboxProfileNotFound      = errors.New("sandbox profile not found")
	ErrSandboxProfileChanged       = errors.New("sandbox profile changed since preview")
	ErrSandboxProfileInvalidImport = errors.New("invalid sandbox profile import")
)

// SandboxFilesystemGrant is one normalized filesystem capability in a sandbox
// profile. Profiles may retain canonical lexical paths that do not yet exist
// locally; resolution preserves those rules for the harness sandbox.
type SandboxFilesystemGrant = sandboxpolicy.FilesystemGrant

// SandboxEnvironmentEntry remains a slice element (rather than a map value)
// so duplicate keys survive decoding long enough for the normalization seam to
// distinguish identical duplicates from conflicting values.
type SandboxEnvironmentEntry = sandboxpolicy.EnvironmentEntry

// SandboxProfile is a stable-ID registry row. Environment values are
// deliberately non-secret plaintext configuration, not a credential store.
type SandboxProfile struct {
	ID          int64                     `json:"id"`
	Name        string                    `json:"name"`
	Filesystem  []SandboxFilesystemGrant  `json:"filesystem"`
	Environment []SandboxEnvironmentEntry `json:"environment"`
	CreatedAt   time.Time                 `json:"created_at"`
	UpdatedAt   time.Time                 `json:"updated_at"`
}

type SandboxProfileAssignments struct {
	Global string
	Groups map[string]string
}

type SandboxProfileImportResult struct {
	Imported []string
	Skipped  []string
	Warnings []string
}

func CreateSandboxProfile(p *SandboxProfile) (int64, error) {
	p, err := normalizeSandboxProfileForStore(p)
	if err != nil {
		return 0, err
	}
	filesystemJSON, environmentJSON, err := marshalSandboxProfilePayload(p)
	if err != nil {
		return 0, err
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	now := time.Now().Format(time.RFC3339Nano)
	res, err := d.Exec(`INSERT INTO sandbox_profiles
		(name, filesystem_json, environment_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		p.Name, filesystemJSON, environmentJSON, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrSandboxProfileNameTaken
		}
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateSandboxProfile atomically replaces the complete payload. The row ID
// is immutable; rename snapshots for both assignment surfaces are refreshed in
// the same transaction.
func UpdateSandboxProfile(p *SandboxProfile) error {
	return updateSandboxProfile(p, "")
}

// UpdateSandboxProfileIfUnchanged applies the complete replacement only when
// updated_at still matches revision. The comparison is part of the UPDATE so
// another writer cannot slip between a handler-side check and the write.
func UpdateSandboxProfileIfUnchanged(p *SandboxProfile, revision string) error {
	return updateSandboxProfile(p, revision)
}

func updateSandboxProfile(p *SandboxProfile, revision string) error {
	p, err := normalizeSandboxProfileForStore(p)
	if err != nil {
		return err
	}
	filesystemJSON, environmentJSON, err := marshalSandboxProfilePayload(p)
	if err != nil {
		return err
	}
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().Format(time.RFC3339Nano)
	query := `UPDATE sandbox_profiles SET name = ?, filesystem_json = ?, environment_json = ?, updated_at = ? WHERE id = ?`
	args := []any{p.Name, filesystemJSON, environmentJSON, now, p.ID}
	if revision != "" {
		query += ` AND updated_at = ?`
		args = append(args, revision)
	}
	res, err := tx.Exec(query, args...)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrSandboxProfileNameTaken
		}
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if revision != "" {
			return ErrSandboxProfileChanged
		}
		return sql.ErrNoRows
	}
	if _, err := tx.Exec(`UPDATE agent_groups SET sandbox_profile = ? WHERE sandbox_profile_id = ?`, p.Name, p.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE sandbox_profile_global_assignment SET profile_name = ? WHERE profile_id = ?`, p.Name, p.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func marshalSandboxProfilePayload(p *SandboxProfile) (string, string, error) {
	filesystem := p.Filesystem
	if filesystem == nil {
		filesystem = []SandboxFilesystemGrant{}
	}
	environment := p.Environment
	if environment == nil {
		environment = []SandboxEnvironmentEntry{}
	}
	filesystemJSON, err := json.Marshal(filesystem)
	if err != nil {
		return "", "", fmt.Errorf("marshal sandbox profile filesystem: %w", err)
	}
	environmentJSON, err := json.Marshal(environment)
	if err != nil {
		return "", "", fmt.Errorf("marshal sandbox profile environment: %w", err)
	}
	return string(filesystemJSON), string(environmentJSON), nil
}

// normalizeSandboxProfileForStore is the single defensive persistence seam
// shared by create/update (and reusable by import). Missing filesystem paths
// are valid profile data: they are retained in canonical lexical form so a
// profile can be prepared before its directories exist. Existing ancestors,
// protected-state paths, and environment entries still receive the full
// validation here. Resolution uses the same persistence normalization so the
// canonical missing rules can be passed through to the harness sandbox.
func normalizeSandboxProfileForStore(p *SandboxProfile) (*SandboxProfile, error) {
	if p == nil {
		return nil, errors.New("sandbox profile is nil")
	}
	normalized, _, err := sandboxpolicy.NormalizeForPersistence(sandboxpolicy.Profile{
		Name: p.Name, Filesystem: p.Filesystem, Environment: p.Environment,
	})
	if err != nil {
		return nil, err
	}
	out := *p
	out.Name = normalized.Name
	out.Filesystem = normalized.Filesystem
	out.Environment = normalized.Environment
	return &out, nil
}

func GetSandboxProfile(name string) (*SandboxProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return scanSandboxProfile(d.QueryRow(sandboxProfileSelect+` WHERE name = ?`, name))
}

func GetSandboxProfileByID(id int64) (*SandboxProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return scanSandboxProfile(d.QueryRow(sandboxProfileSelect+` WHERE id = ?`, id))
}

const sandboxProfileSelect = `SELECT id, name, filesystem_json, environment_json, created_at, updated_at FROM sandbox_profiles`

func scanSandboxProfile(row rowScanner) (*SandboxProfile, error) {
	var p SandboxProfile
	var filesystemJSON, environmentJSON, createdAt, updatedAt string
	if err := row.Scan(&p.ID, &p.Name, &filesystemJSON, &environmentJSON, &createdAt, &updatedAt); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(filesystemJSON), &p.Filesystem); err != nil {
		return nil, fmt.Errorf("decode sandbox profile %q filesystem: %w", p.Name, err)
	}
	if err := json.Unmarshal([]byte(environmentJSON), &p.Environment); err != nil {
		return nil, fmt.Errorf("decode sandbox profile %q environment: %w", p.Name, err)
	}
	if p.Filesystem == nil {
		p.Filesystem = []SandboxFilesystemGrant{}
	}
	if p.Environment == nil {
		p.Environment = []SandboxEnvironmentEntry{}
	}
	p.CreatedAt = parseTimeOrZero(createdAt)
	p.UpdatedAt = parseTimeOrZero(updatedAt)
	// These paths were canonical at persistence time, but that is not a durable
	// authorization proof: directories can be replaced by symlinks later. The
	// TCL-320 launch/application boundary must call sandboxpolicy.Normalize
	// again immediately before rendering any harness grant.
	return &p, nil
}

func ListSandboxProfiles() ([]*SandboxProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(sandboxProfileSelect + ` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*SandboxProfile{}
	for rows.Next() {
		p, err := scanSandboxProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ImportSandboxProfiles validates and applies a portable profile bundle in one
// transaction. Expected conflicts are resolved before the first write, and
// optional assignment restoration rides the same commit, so an error never
// leaves a partially imported registry.
func ImportSandboxProfiles(profiles []*SandboxProfile, onConflict string, assignments *SandboxProfileAssignments) (SandboxProfileImportResult, error) {
	result := SandboxProfileImportResult{Imported: []string{}, Skipped: []string{}, Warnings: []string{}}
	onConflict = strings.ToLower(strings.TrimSpace(onConflict))
	if onConflict == "" {
		onConflict = "error"
	}
	if onConflict != "error" && onConflict != "skip" && onConflict != "overwrite" {
		return result, fmt.Errorf("%w: on_conflict must be error, skip, or overwrite", ErrSandboxProfileInvalidImport)
	}
	normalized := make([]*SandboxProfile, 0, len(profiles))
	missingByName := make(map[string][]string, len(profiles))
	seen := map[string]bool{}
	for i, profile := range profiles {
		if profile == nil {
			return result, fmt.Errorf("%w: profile #%d: sandbox profile is nil", ErrSandboxProfileInvalidImport, i+1)
		}
		p, missing, err := sandboxpolicy.NormalizeForImport(sandboxpolicy.Profile{
			Name: profile.Name, Filesystem: profile.Filesystem, Environment: profile.Environment,
		})
		if err != nil {
			return result, fmt.Errorf("%w: profile #%d: %v", ErrSandboxProfileInvalidImport, i+1, err)
		}
		normalizedProfile := *profile
		normalizedProfile.Name = p.Name
		normalizedProfile.Filesystem = p.Filesystem
		normalizedProfile.Environment = p.Environment
		if seen[normalizedProfile.Name] {
			return result, fmt.Errorf("%w: sandbox profile %q appears more than once", ErrSandboxProfileInvalidImport, normalizedProfile.Name)
		}
		seen[normalizedProfile.Name] = true
		missingByName[normalizedProfile.Name] = missing
		normalized = append(normalized, &normalizedProfile)
	}

	d, err := Open()
	if err != nil {
		return result, err
	}
	tx, err := d.Begin()
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	type planned struct {
		profile    *SandboxProfile
		existingID int64
	}
	plans := make([]planned, 0, len(normalized))
	for _, profile := range normalized {
		var id int64
		err := tx.QueryRow(`SELECT id FROM sandbox_profiles WHERE name = ?`, profile.Name).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return result, err
		}
		if err == nil && onConflict == "error" {
			return result, fmt.Errorf("%w: %q", ErrSandboxProfileNameTaken, profile.Name)
		}
		plans = append(plans, planned{profile: profile, existingID: id})
	}

	now := time.Now().Format(time.RFC3339Nano)
	for _, item := range plans {
		if item.existingID != 0 && onConflict == "skip" {
			result.Skipped = append(result.Skipped, item.profile.Name)
			continue
		}
		for _, path := range missingByName[item.profile.Name] {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"sandbox profile %q path %q does not exist locally; the rule will target it if created", item.profile.Name, path))
		}
		filesystemJSON, environmentJSON, err := marshalSandboxProfilePayload(item.profile)
		if err != nil {
			return result, err
		}
		if item.existingID != 0 {
			if _, err := tx.Exec(`UPDATE sandbox_profiles SET filesystem_json = ?, environment_json = ?, updated_at = ? WHERE id = ?`,
				filesystemJSON, environmentJSON, now, item.existingID); err != nil {
				return result, err
			}
		} else if _, err := tx.Exec(`INSERT INTO sandbox_profiles
			(name, filesystem_json, environment_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			item.profile.Name, filesystemJSON, environmentJSON, now, now); err != nil {
			if isUniqueViolation(err) {
				return result, ErrSandboxProfileNameTaken
			}
			return result, err
		}
		result.Imported = append(result.Imported, item.profile.Name)
	}

	if assignments != nil {
		if assignments.Global != "" {
			var id int64
			if err := tx.QueryRow(`SELECT id FROM sandbox_profiles WHERE name = ?`, assignments.Global).Scan(&id); errors.Is(err, sql.ErrNoRows) {
				result.Warnings = append(result.Warnings, fmt.Sprintf("global assignment references missing sandbox profile %q", assignments.Global))
			} else if err != nil {
				return result, err
			} else if _, err := tx.Exec(`INSERT OR REPLACE INTO sandbox_profile_global_assignment (id, profile_name, profile_id) VALUES (1, ?, ?)`, assignments.Global, id); err != nil {
				return result, err
			}
		}
		groups := make([]string, 0, len(assignments.Groups))
		for group := range assignments.Groups {
			groups = append(groups, group)
		}
		sort.Strings(groups)
		for _, group := range groups {
			profile := assignments.Groups[group]
			var groupID, profileID int64
			if err := tx.QueryRow(`SELECT id FROM agent_groups WHERE name = ?`, group).Scan(&groupID); errors.Is(err, sql.ErrNoRows) {
				result.Warnings = append(result.Warnings, fmt.Sprintf("group assignment skipped: no group %q", group))
				continue
			} else if err != nil {
				return result, err
			}
			if err := tx.QueryRow(`SELECT id FROM sandbox_profiles WHERE name = ?`, profile).Scan(&profileID); errors.Is(err, sql.ErrNoRows) {
				result.Warnings = append(result.Warnings, fmt.Sprintf("group %q assignment references missing sandbox profile %q", group, profile))
				continue
			} else if err != nil {
				return result, err
			}
			if _, err := tx.Exec(`UPDATE agent_groups SET sandbox_profile = ?, sandbox_profile_id = ? WHERE id = ?`, profile, profileID, groupID); err != nil {
				return result, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func DeleteSandboxProfile(name string) (int64, error) {
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
	if err := tx.QueryRow(`SELECT id FROM sandbox_profiles WHERE name = ?`, name).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE agent_groups SET sandbox_profile = '', sandbox_profile_id = NULL WHERE sandbox_profile_id = ?`, id); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM sandbox_profile_global_assignment WHERE profile_id = ?`, id); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM sandbox_profiles WHERE id = ?`, id)
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

// SetGlobalSandboxProfile sets the single durable operator-wide assignment. A
// blank name clears it; non-blank names must resolve to an existing profile.
func SetGlobalSandboxProfile(name string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if name == "" {
		if _, err := tx.Exec(`DELETE FROM sandbox_profile_global_assignment WHERE id = 1`); err != nil {
			return err
		}
		return tx.Commit()
	}
	var id int64
	if err := tx.QueryRow(`SELECT id FROM sandbox_profiles WHERE name = ?`, name).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return ErrSandboxProfileNotFound
	} else if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO sandbox_profile_global_assignment (id, profile_name, profile_id) VALUES (1, ?, ?)`, name, id); err != nil {
		return err
	}
	return tx.Commit()
}

func GetGlobalSandboxProfile() (*SandboxProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var id int64
	if err := d.QueryRow(`SELECT profile_id FROM sandbox_profile_global_assignment WHERE id = 1`).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return GetSandboxProfileByID(id)
}

func SetAgentGroupSandboxProfile(group, name string) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var profileID sql.NullInt64
	if name != "" {
		profileID, err = registryIDByName(tx, "sandbox_profiles", name)
		if err != nil {
			return 0, err
		}
		if !profileID.Valid {
			return 0, ErrSandboxProfileNotFound
		}
	}
	res, err := tx.Exec(`UPDATE agent_groups SET sandbox_profile = ?, sandbox_profile_id = ? WHERE name = ?`, name, profileID, group)
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

func GetAgentGroupSandboxProfile(group string) (*SandboxProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var id sql.NullInt64
	if err := d.QueryRow(`SELECT sandbox_profile_id FROM agent_groups WHERE name = ?`, group).Scan(&id); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if !id.Valid {
		return nil, nil
	}
	return GetSandboxProfileByID(id.Int64)
}

// ResolveEffectiveSandboxSnapshot atomically reads the stable global/group
// assignments plus an optional explicit human selection, then freezes their
// composed values. Mutable profile references are never returned as launch
// authority: only the versioned value snapshot is.
func ResolveEffectiveSandboxSnapshot(groupID int64, explicitName string) (sandboxpolicy.Snapshot, error) {
	d, err := Open()
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	tx, err := d.Begin()
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()

	loadByID := func(id int64) (*SandboxProfile, error) {
		if id == 0 {
			return nil, nil
		}
		profile, err := scanSandboxProfile(tx.QueryRow(sandboxProfileSelect+` WHERE id = ?`, id))
		if err != nil {
			return nil, err
		}
		if profile == nil {
			return nil, fmt.Errorf("sandbox profile id %d referenced by assignment was not found", id)
		}
		return profile, nil
	}
	var globalID, groupProfileID int64
	if err := tx.QueryRow(`SELECT COALESCE((SELECT profile_id FROM sandbox_profile_global_assignment WHERE id = 1), 0)`).Scan(&globalID); err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	if groupID > 0 {
		if err := tx.QueryRow(`SELECT COALESCE(sandbox_profile_id, 0) FROM agent_groups WHERE id = ?`, groupID).Scan(&groupProfileID); errors.Is(err, sql.ErrNoRows) {
			return sandboxpolicy.Snapshot{}, fmt.Errorf("agent group %d not found", groupID)
		} else if err != nil {
			return sandboxpolicy.Snapshot{}, err
		}
	}
	global, err := loadByID(globalID)
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	group, err := loadByID(groupProfileID)
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	var explicit *SandboxProfile
	explicitName = strings.TrimSpace(explicitName)
	if explicitName != "" {
		explicit, err = scanSandboxProfile(tx.QueryRow(sandboxProfileSelect+` WHERE name = ?`, explicitName))
		if errors.Is(err, sql.ErrNoRows) || explicit == nil {
			return sandboxpolicy.Snapshot{}, ErrSandboxProfileNotFound
		}
		if err != nil {
			return sandboxpolicy.Snapshot{}, err
		}
	}

	toPolicy := func(p *SandboxProfile) *sandboxpolicy.Profile {
		if p == nil {
			return nil
		}
		return &sandboxpolicy.Profile{Name: p.Name, Filesystem: p.Filesystem, Environment: p.Environment}
	}
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{
		Global: toPolicy(global), Group: toPolicy(group), Explicit: toPolicy(explicit),
	})
	if err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	applied := make([]sandboxpolicy.AppliedProfile, 0, 3)
	for _, item := range []struct {
		scope   sandboxpolicy.Scope
		profile *SandboxProfile
	}{
		{sandboxpolicy.ScopeGlobal, global},
		{sandboxpolicy.ScopeGroup, group},
		{sandboxpolicy.ScopeExplicit, explicit},
	} {
		if item.profile != nil {
			applied = append(applied, sandboxpolicy.AppliedProfile{
				Scope: item.scope, ID: item.profile.ID, Name: item.profile.Name, UpdatedAt: item.profile.UpdatedAt,
			})
		}
	}
	if err := tx.Commit(); err != nil {
		return sandboxpolicy.Snapshot{}, err
	}
	return sandboxpolicy.NewSnapshot(effective, applied), nil
}
