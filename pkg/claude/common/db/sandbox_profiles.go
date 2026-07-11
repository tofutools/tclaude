package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

var (
	ErrSandboxProfileNameTaken = errors.New("a sandbox profile with that name already exists")
	ErrSandboxProfileNotFound  = errors.New("sandbox profile not found")
	ErrSandboxProfileConflict  = errors.New("sandbox profile contains conflicting duplicate entries")
)

// SandboxFilesystemGrant is one canonical filesystem capability in a sandbox
// profile. Validation and canonicalization live at the trusted domain/API
// boundary; this store round-trips that normalized payload without widening it.
type SandboxFilesystemGrant struct {
	Path   string `json:"path"`
	Access string `json:"access"`
}

// SandboxEnvironmentEntry remains a slice element (rather than a map value)
// so duplicate keys survive decoding long enough for the normalization seam to
// distinguish identical duplicates from conflicting values.
type SandboxEnvironmentEntry struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

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
	res, err := tx.Exec(`UPDATE sandbox_profiles SET name = ?, filesystem_json = ?, environment_json = ?, updated_at = ? WHERE id = ?`,
		p.Name, filesystemJSON, environmentJSON, now, p.ID)
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
// shared by create/update (and reusable by import). The policy layer may do
// filesystem-dependent canonicalization first; this final invariant never
// mutates caller-owned slices, validates the closed access enum, folds exact
// duplicates deterministically, applies write-dominates-read for an already
// canonical path, and rejects conflicting environment values.
func normalizeSandboxProfileForStore(p *SandboxProfile) (*SandboxProfile, error) {
	if p == nil {
		return nil, errors.New("sandbox profile is nil")
	}
	out := *p
	out.Filesystem = append([]SandboxFilesystemGrant(nil), p.Filesystem...)
	out.Environment = append([]SandboxEnvironmentEntry(nil), p.Environment...)
	sort.Slice(out.Filesystem, func(i, j int) bool {
		if out.Filesystem[i].Path == out.Filesystem[j].Path {
			return out.Filesystem[i].Access < out.Filesystem[j].Access
		}
		return out.Filesystem[i].Path < out.Filesystem[j].Path
	})
	filesystem := make([]SandboxFilesystemGrant, 0, len(out.Filesystem))
	for _, grant := range out.Filesystem {
		if grant.Access != "read" && grant.Access != "write" {
			return nil, fmt.Errorf("filesystem grant %q has invalid access %q (want read or write)", grant.Path, grant.Access)
		}
		if len(filesystem) > 0 && filesystem[len(filesystem)-1].Path == grant.Path {
			if grant.Access == "write" {
				filesystem[len(filesystem)-1] = grant
			}
			continue
		}
		filesystem = append(filesystem, grant)
	}
	sort.Slice(out.Environment, func(i, j int) bool {
		if out.Environment[i].Name == out.Environment[j].Name {
			return out.Environment[i].Value < out.Environment[j].Value
		}
		return out.Environment[i].Name < out.Environment[j].Name
	})
	environment := make([]SandboxEnvironmentEntry, 0, len(out.Environment))
	for _, entry := range out.Environment {
		if len(environment) > 0 && environment[len(environment)-1].Name == entry.Name {
			if environment[len(environment)-1].Value != entry.Value {
				return nil, fmt.Errorf("%w: environment key %q has multiple values", ErrSandboxProfileConflict, entry.Name)
			}
			continue
		}
		environment = append(environment, entry)
	}
	out.Filesystem = filesystem
	out.Environment = environment
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
	if name == "" {
		_, err := d.Exec(`DELETE FROM sandbox_profile_global_assignment WHERE id = 1`)
		return err
	}
	p, err := GetSandboxProfile(name)
	if err != nil {
		return err
	}
	if p == nil {
		return ErrSandboxProfileNotFound
	}
	_, err = d.Exec(`INSERT OR REPLACE INTO sandbox_profile_global_assignment (id, profile_name, profile_id) VALUES (1, ?, ?)`, p.Name, p.ID)
	return err
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
	var profileID sql.NullInt64
	if name != "" {
		profileID, err = registryIDByNameDB(d, "sandbox_profiles", name)
		if err != nil {
			return 0, err
		}
		if !profileID.Valid {
			return 0, ErrSandboxProfileNotFound
		}
	}
	res, err := d.Exec(`UPDATE agent_groups SET sandbox_profile = ?, sandbox_profile_id = ? WHERE name = ?`, name, profileID, group)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
