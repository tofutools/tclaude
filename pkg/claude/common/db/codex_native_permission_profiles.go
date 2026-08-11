package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type CodexNativePermissionProfile struct {
	Generation     string
	ProfileName    string
	ProfileTOML    string
	CleanupPending bool
	CreatedAt      time.Time
	OwnerAgentID   string
	OwnerConvID    string
	LaunchID       string
	LaunchReady    bool
}

// bindCodexNativePermissionProfileOwnerTx completes ownership for a profile
// registered at the launch boundary, before Codex has revealed its conv-id.
// The stable agent may still be unknown here; enrollment fills that companion
// through propagateAgentCompanions below.
func bindCodexNativePermissionProfileOwnerTx(x dbExecQuerier, launchID, convID string) error {
	launchID = strings.TrimSpace(launchID)
	convID = strings.TrimSpace(convID)
	if launchID == "" || convID == "" {
		return nil
	}
	var has int
	if err := x.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('codex_native_permission_profiles') WHERE name = 'owner_conv_id'`).Scan(&has); err != nil {
		return fmt.Errorf("bind codex native profile owner (probe): %w", err)
	}
	if has == 0 {
		return nil
	}
	if _, err := x.Exec(`UPDATE codex_native_permission_profiles
		SET owner_conv_id = ?,
		    owner_agent_id = CASE WHEN `+agentForConvExpr+` <> '' THEN `+agentForConvExpr+` ELSE owner_agent_id END
		WHERE launch_id = ?`, convID, convID, convID, launchID); err != nil {
		return fmt.Errorf("bind codex native profile owner: %w", err)
	}
	return nil
}

func propagateCodexNativePermissionProfileAgent(x dbExecQuerier, convID, agentID string) error {
	if convID == "" || agentID == "" {
		return nil
	}
	var has int
	if err := x.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('codex_native_permission_profiles') WHERE name = 'owner_agent_id'`).Scan(&has); err != nil {
		return fmt.Errorf("propagate codex native profile agent (probe): %w", err)
	}
	if has == 0 {
		return nil
	}
	if _, err := x.Exec(`UPDATE codex_native_permission_profiles SET owner_agent_id = ?
		WHERE owner_conv_id = ? AND owner_agent_id = ''`, agentID, convID); err != nil {
		return fmt.Errorf("propagate codex native profile agent: %w", err)
	}
	return nil
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
		(generation, profile_name, profile_toml, cleanup_pending, created_at,
		 owner_agent_id, owner_conv_id, launch_id, launch_ready) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(generation) DO UPDATE SET profile_name=excluded.profile_name,
		profile_toml=excluded.profile_toml, cleanup_pending=excluded.cleanup_pending,
		owner_agent_id=excluded.owner_agent_id, owner_conv_id=excluded.owner_conv_id,
		launch_id=excluded.launch_id, launch_ready=excluded.launch_ready`,
		profile.Generation, profile.ProfileName, profile.ProfileTOML, profile.CleanupPending,
		dbTime(profile.CreatedAt), strings.TrimSpace(profile.OwnerAgentID),
		strings.TrimSpace(profile.OwnerConvID), strings.TrimSpace(profile.LaunchID), profile.LaunchReady)
	return err
}

// MarkCodexNativePermissionProfileLaunchReady publishes the durable readiness
// fact used to retire older ordinary (non-app-server) launch definitions.
func MarkCodexNativePermissionProfileLaunchReady(generation string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	result, err := d.Exec(`UPDATE codex_native_permission_profiles SET launch_ready = 1
		WHERE generation = ? AND cleanup_pending = 0`, strings.TrimSpace(generation))
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func MarkCodexNativePermissionProfileCleanupPending(generation string) error {
	return MarkCodexNativePermissionProfilesCleanupPending([]string{generation})
}

// MarkCodexNativePermissionProfilesCleanupPending durably records an exact
// cleanup set in one transaction. The registry publisher may fail or the
// process may exit after this commit; startup reconciliation will still retry
// every marked generation without reconstructing ownership from files.
func MarkCodexNativePermissionProfilesCleanupPending(generations []string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, generation := range generations {
		generation = strings.TrimSpace(generation)
		if generation == "" {
			continue
		}
		if _, err = tx.Exec(`UPDATE codex_native_permission_profiles SET cleanup_pending = 1
			WHERE generation = ?`, generation); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	err = d.QueryRow(`SELECT generation, profile_name, profile_toml, cleanup_pending, created_at,
		owner_agent_id, owner_conv_id, launch_id, launch_ready
		FROM codex_native_permission_profiles WHERE generation = ?`, strings.TrimSpace(generation)).
		Scan(&profile.Generation, &profile.ProfileName, &profile.ProfileTOML, &profile.CleanupPending, &created,
			&profile.OwnerAgentID, &profile.OwnerConvID, &profile.LaunchID, &profile.LaunchReady)
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
	rows, err := d.Query(`SELECT generation, profile_name, profile_toml, cleanup_pending, created_at,
		owner_agent_id, owner_conv_id, launch_id, launch_ready
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
			&profile.CleanupPending, &created, &profile.OwnerAgentID, &profile.OwnerConvID,
			&profile.LaunchID, &profile.LaunchReady); err != nil {
			return nil, err
		}
		profile.CreatedAt = created.Time()
		out = append(out, profile)
	}
	return out, rows.Err()
}

// ListCodexNativePermissionProfileGenerationsForConv returns only tclaude's
// exact managed generation keys. It never infers names from config.toml, which
// keeps lifecycle cleanup inside the managed namespace.
func ListCodexNativePermissionProfileGenerationsForConv(convID string) ([]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT DISTINCT profile.generation
		FROM codex_native_permission_profiles profile
		LEFT JOIN codex_app_server_runtimes runtime ON runtime.generation = profile.generation
		WHERE profile.owner_conv_id = ? OR runtime.conv_id = ? ORDER BY profile.created_at ASC`,
		strings.TrimSpace(convID), strings.TrimSpace(convID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var generation string
		if err := rows.Scan(&generation); err != nil {
			return nil, err
		}
		out = append(out, generation)
	}
	return out, rows.Err()
}

// ListCodexNativePermissionProfileGenerationsForAgent returns every managed
// generation owned by one stable actor, including predecessor conversations.
func ListCodexNativePermissionProfileGenerationsForAgent(agentID string) ([]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT DISTINCT profile.generation
		FROM codex_native_permission_profiles profile
		LEFT JOIN codex_app_server_runtimes runtime ON runtime.generation = profile.generation
		WHERE profile.owner_agent_id = ? OR runtime.agent_id = ? ORDER BY profile.created_at ASC`,
		strings.TrimSpace(agentID), strings.TrimSpace(agentID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var generation string
		if err := rows.Scan(&generation); err != nil {
			return nil, err
		}
		out = append(out, generation)
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
		) OR EXISTS (
			SELECT 1 FROM codex_native_permission_profiles replacement
			WHERE replacement.generation <> codex_native_permission_profiles.generation
			  AND replacement.cleanup_pending = 0 AND replacement.launch_ready = 1
			  AND replacement.created_at > codex_native_permission_profiles.created_at
			  AND ((codex_native_permission_profiles.owner_agent_id <> '' AND
			        replacement.owner_agent_id = codex_native_permission_profiles.owner_agent_id)
			       OR (codex_native_permission_profiles.owner_agent_id = '' AND
			           codex_native_permission_profiles.owner_conv_id <> '' AND
			           replacement.owner_conv_id = codex_native_permission_profiles.owner_conv_id))
		)`, CodexAppServerReady)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
