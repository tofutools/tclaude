package db

import (
	"database/sql"
	"fmt"
)

// migrateV191toV192 adds the per-launch Copilot API runtime record: the
// loopback port agentd allocated for one `copilot --ui-server` pane.
//
// A table of its own rather than a sessions column, because the port is
// PER-LAUNCH state and not durable launch posture. The posture ("this agent is
// driven over the API") already lives in the versioned relaunch-profile JSON
// and must survive a relaunch; the port must NOT — a relaunched pane binds a
// newly allocated one, and a stale number surviving into the next launch is
// exactly the lie that makes debugging worse. Deleting the row is therefore
// meaningful, which a column would not make so.
//
// Keyed by CONVERSATION, not by tclaude session. The conversation id is what
// agentd holds on both launch paths — a fresh spawn presets it so the agent can
// be enrolled before the pane starts, and a resume names it — whereas a resume
// mints a fresh session label inside the forked `session new`, where agentd
// cannot see it. One conversation has one current port, and a relaunch replaces
// it, which is exactly the lifetime being modelled.
//
// The row records what a launch was TOLD to use, not a port proven reachable.
// Reachability and listener ownership are established per use, against the live
// process, and are deliberately not persisted as a claim: `--ui-server` has no
// authentication (TCL-1055), so a stored "verified" bit would age into a
// statement about whatever holds the port now. See TCL-1054.
func migrateV191toV192(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v191→v192 (Copilot API runtimes): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS copilot_api_runtimes (
			conv_id TEXT PRIMARY KEY,
			port INTEGER NOT NULL CHECK (port > 0 AND port <= 65535),
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		) STRICT
	`); err != nil {
		return fmt.Errorf("migrate v191→v192 (Copilot API runtimes): create table: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 192`); err != nil {
		return fmt.Errorf("migrate v191→v192 (Copilot API runtimes): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v191→v192 (Copilot API runtimes): commit: %w", err)
	}
	return nil
}
