package db

import (
	"errors"
	"strings"
	"time"
)

type CodexNativePermissionProfile struct {
	Generation  string
	ProfileName string
	ProfileTOML string
	CreatedAt   time.Time
}

func UpsertCodexNativePermissionProfile(profile CodexNativePermissionProfile) error {
	profile.Generation = strings.TrimSpace(profile.Generation)
	profile.ProfileName = strings.TrimSpace(profile.ProfileName)
	if profile.Generation == "" || profile.ProfileName == "" || strings.TrimSpace(profile.ProfileTOML) == "" {
		return errors.New("Codex native permission profile needs generation, name, and complete TOML")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	if profile.CreatedAt.IsZero() {
		profile.CreatedAt = time.Now().UTC()
	}
	_, err = d.Exec(`INSERT INTO codex_native_permission_profiles
		(generation, profile_name, profile_toml, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(generation) DO UPDATE SET profile_name=excluded.profile_name,
		profile_toml=excluded.profile_toml`, profile.Generation, profile.ProfileName,
		profile.ProfileTOML, dbTime(profile.CreatedAt))
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

func ListCodexNativePermissionProfiles() ([]CodexNativePermissionProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT generation, profile_name, profile_toml, created_at
		FROM codex_native_permission_profiles ORDER BY profile_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CodexNativePermissionProfile
	for rows.Next() {
		var profile CodexNativePermissionProfile
		var created dbTimestamp
		if err := rows.Scan(&profile.Generation, &profile.ProfileName, &profile.ProfileTOML, &created); err != nil {
			return nil, err
		}
		profile.CreatedAt = created.Time()
		out = append(out, profile)
	}
	return out, rows.Err()
}
