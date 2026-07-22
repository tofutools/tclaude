package db

import (
	"database/sql"
	"fmt"
)

// migrateV144toV145 moves managed-agent relaunch authority to records whose
// lifetime matches the state they describe:
//
//   - agents.relaunch_profile owns mutable agent launch intent; and
//   - conversation_resume_profiles owns the harness conversation's resume
//     identity, physical provenance, and a fallback launch snapshot for plain
//     conversations, independently of agent enrollment.
//
// sessions keep their existing launch snapshots for history, standalone CLI
// views, and one-time legacy backfill. They are no longer required to survive
// after the durable profiles have been populated.
func migrateV144toV145(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v144→v145 (durable relaunch profiles): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveAgents, err := txTableExists(tx, "agents")
	if err != nil {
		return fmt.Errorf("migrate v144→v145 (probe agents): %w", err)
	}
	var haveInitialSpawnConfig int
	if haveAgents {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('agents')
			WHERE name = 'initial_spawn_config'`).Scan(&haveInitialSpawnConfig); err != nil {
			return fmt.Errorf("migrate v144→v145 (probe agents.initial_spawn_config): %w", err)
		}
	}
	if haveAgents {
		var haveColumn int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('agents') WHERE name = 'relaunch_profile'`,
		).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v144→v145 (probe agents.relaunch_profile): %w", err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE agents ADD COLUMN relaunch_profile TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("migrate v144→v145 (add agents.relaunch_profile): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS conversation_resume_profiles (
		conv_id      TEXT PRIMARY KEY,
		profile_json TEXT NOT NULL,
		updated_at   TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("migrate v144→v145 (create conversation resume profiles): %w", err)
	}

	// Seed explicit birth-time agent settings first. A newer session snapshot
	// below replaces them when one exists; for already-pruned legacy agents the
	// explicit fields remain useful evidence while omitted fields stay unknown.
	if haveAgents && haveInitialSpawnConfig != 0 {
		rows, err := tx.Query(`SELECT agent_id, initial_spawn_config FROM agents
			WHERE relaunch_profile = '' AND initial_spawn_config <> ''`)
		if err != nil {
			return fmt.Errorf("migrate v144→v145 (list initial spawn configs): %w", err)
		}
		var seeds []struct{ agentID, raw string }
		for rows.Next() {
			var seed struct{ agentID, raw string }
			if err := rows.Scan(&seed.agentID, &seed.raw); err != nil {
				_ = rows.Close()
				return fmt.Errorf("migrate v144→v145 (scan initial spawn config): %w", err)
			}
			seeds = append(seeds, seed)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("migrate v144→v145 (close initial spawn configs): %w", err)
		}
		for _, seed := range seeds {
			if err := seedAgentRelaunchProfileFromSpawnConfigTx(tx, seed.agentID, seed.raw); err != nil {
				return fmt.Errorf("migrate v144→v145 (seed agent %s): %w", seed.agentID, err)
			}
		}
	}

	// Oldest to newest makes the newest legacy session snapshot win per
	// conversation. projectSessionRelaunchProfilesTx also fills the owning agent
	// when the conversation is enrolled.
	haveSessions, err := txTableExists(tx, "sessions")
	if err != nil {
		return fmt.Errorf("migrate v144→v145 (probe sessions): %w", err)
	}
	// Historical half-applied migration fixtures can carry a deliberately
	// skeletal sessions table all the way to the current head. The profile
	// tables are still valid in that state; skip evidence projection unless the
	// full launch snapshot is present.
	var sessionLaunchColumns int
	if haveSessions {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name IN (
			'id', 'conv_id', 'cwd', 'harness', 'sandbox_mode', 'approval_policy',
			'approval_auto_review', 'model_id', 'effort_level', 'context_window_size',
			'ask_user_question_timeout', 'remote_control', 'auto_memory',
			'resume_provenance', 'updated_at')`).Scan(&sessionLaunchColumns); err != nil {
			return fmt.Errorf("migrate v144→v145 (probe session launch columns): %w", err)
		}
	}
	if haveSessions && sessionLaunchColumns == 15 {
		rows, err := tx.Query(`SELECT id FROM sessions ORDER BY julianday(created_at), rowid`)
		if err != nil {
			return fmt.Errorf("migrate v144→v145 (list sessions): %w", err)
		}
		var sessionIDs []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return fmt.Errorf("migrate v144→v145 (scan session): %w", err)
			}
			sessionIDs = append(sessionIDs, id)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("migrate v144→v145 (close sessions): %w", err)
		}
		for _, id := range sessionIDs {
			if err := projectSessionRelaunchProfilesTx(tx, id, relaunchProjectionOptions{
				RemoteControl: true, AutoMemory: true,
			}); err != nil {
				return fmt.Errorf("migrate v144→v145 (backfill session %s): %w", id, err)
			}
		}

		// An old generation can have a newer updated_at than the actor's head
		// (for example after reaper reconciliation). Re-project each current head
		// last so agent policy always comes from agents.current_conv_id.
		if haveAgents {
			rows, err := tx.Query(`SELECT current_conv_id FROM agents ORDER BY rowid`)
			if err != nil {
				return fmt.Errorf("migrate v144→v145 (list agent heads): %w", err)
			}
			var heads []string
			for rows.Next() {
				var convID string
				if err := rows.Scan(&convID); err != nil {
					_ = rows.Close()
					return fmt.Errorf("migrate v144→v145 (scan agent head): %w", err)
				}
				heads = append(heads, convID)
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("migrate v144→v145 (close agent heads): %w", err)
			}
			for _, convID := range heads {
				if err := projectLatestSessionRelaunchProfilesForConvTx(tx, convID); err != nil {
					return fmt.Errorf("migrate v144→v145 (backfill agent head %s): %w", convID, err)
				}
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 145`); err != nil {
		return fmt.Errorf("migrate v144→v145 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v144→v145 (commit): %w", err)
	}
	return nil
}
