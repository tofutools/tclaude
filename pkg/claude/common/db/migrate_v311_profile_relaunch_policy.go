package db

import (
	"database/sql"
	"fmt"
)

// migrateV310toV311 adds spawn_profiles.relaunch_policy (launch slice 11,
// tcl-4rd995): a JSON object of relaunch-policy field → mode the profile
// carries. A spawn copies it into the agent's launch layers
// (AgentLaunchLayers.RelaunchOverrides), where it overrides the global
// relaunch policy for that agent. "" is no rows: the global policy applies.
//
// Additive and probe-guarded, so a half-applied run converges on re-run.
func migrateV310toV311(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v310→v311 (profile relaunch policy): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveTable, haveColumn int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'spawn_profiles'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v310→v311 (probe spawn_profiles): %w", err)
	}
	if haveTable > 0 {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('spawn_profiles') WHERE name = 'relaunch_policy'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v310→v311 (probe spawn_profiles.relaunch_policy): %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE spawn_profiles ADD COLUMN relaunch_policy TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("migrate v310→v311 (add spawn_profiles.relaunch_policy): %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 311`); err != nil {
		return fmt.Errorf("migrate v310→v311: %w", err)
	}
	return tx.Commit()
}
