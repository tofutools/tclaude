package db

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

type CodexNativePermissionProfile struct {
	Generation     string
	ProfileName    string
	ProfileTOML    string
	CleanupPending bool
	CreatedAt      time.Time
}

func UpsertCodexNativePermissionProfile(profile CodexNativePermissionProfile) error {
	profile.Generation = strings.TrimSpace(profile.Generation)
	profile.ProfileName = strings.TrimSpace(profile.ProfileName)
	if profile.Generation == "" || profile.ProfileName == "" || strings.TrimSpace(profile.ProfileTOML) == "" {
		return errors.New("codex native permission profile needs generation, name, and complete TOML")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	if profile.CreatedAt.IsZero() {
		profile.CreatedAt = time.Now().UTC()
	}
	_, err = d.Exec(`INSERT INTO codex_native_permission_profiles
		(generation, profile_name, profile_toml, cleanup_pending, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(generation) DO UPDATE SET profile_name=excluded.profile_name,
		profile_toml=excluded.profile_toml, cleanup_pending=excluded.cleanup_pending`,
		profile.Generation, profile.ProfileName, profile.ProfileTOML, profile.CleanupPending,
		dbTime(profile.CreatedAt))
	return err
}

func MarkCodexNativePermissionProfileCleanupPending(generation string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE codex_native_permission_profiles SET cleanup_pending = 1
		WHERE generation = ?`, strings.TrimSpace(generation))
	return err
}

func DeletePendingCodexNativePermissionProfiles() error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM codex_native_permission_profiles WHERE cleanup_pending = 1`)
	return err
}

func DeleteCodexNativePermissionProfile(generation string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM codex_native_permission_profiles WHERE generation = ?`, strings.TrimSpace(generation))
	return err
}

func GetCodexNativePermissionProfile(generation string) (*CodexNativePermissionProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var profile CodexNativePermissionProfile
	var created dbTimestamp
	err = d.QueryRow(`SELECT generation, profile_name, profile_toml, cleanup_pending, created_at
		FROM codex_native_permission_profiles WHERE generation = ?`, strings.TrimSpace(generation)).
		Scan(&profile.Generation, &profile.ProfileName, &profile.ProfileTOML, &profile.CleanupPending, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	profile.CreatedAt = created.Time()
	return &profile, nil
}

func ListCodexNativePermissionProfiles() ([]CodexNativePermissionProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT generation, profile_name, profile_toml, cleanup_pending, created_at
		FROM codex_native_permission_profiles ORDER BY profile_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CodexNativePermissionProfile
	for rows.Next() {
		var profile CodexNativePermissionProfile
		var created dbTimestamp
		if err := rows.Scan(&profile.Generation, &profile.ProfileName, &profile.ProfileTOML,
			&profile.CleanupPending, &created); err != nil {
			return nil, err
		}
		profile.CreatedAt = created.Time()
		out = append(out, profile)
	}
	return out, rows.Err()
}

// PruneSupersededCodexNativePermissionProfiles removes only generations that
// have a newer verified-ready app-server runtime for the same stable agent.
// A newest stopped/dead generation remains resumable and therefore retained;
// an unavailable replacement cannot evict the predecessor that can recover it.
func PruneSupersededCodexNativePermissionProfiles() (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	result, err := d.Exec(`DELETE FROM codex_native_permission_profiles
		WHERE EXISTS (
			SELECT 1 FROM codex_app_server_runtimes current
			WHERE current.generation = codex_native_permission_profiles.generation
			  AND EXISTS (
				SELECT 1 FROM codex_app_server_runtimes replacement
				WHERE replacement.agent_id = current.agent_id
				  AND replacement.generation <> current.generation
				  AND replacement.state = ?
				  AND replacement.created_at > current.created_at
			  )
		)`, CodexAppServerReady)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
