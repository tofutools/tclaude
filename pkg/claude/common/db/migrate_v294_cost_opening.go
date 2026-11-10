package db

import "database/sql"

// migrateV293toV294 adds the stored opening of the cost delta walk, see
// cost_opening.go. It stays empty until the daemon's first refresh, and readers
// use the full history until then, so an upgrade changes no total. The triggers
// mark it stale when any row older than its day changes in a way the walk or
// the Costs view could see.
func migrateV293toV294(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE cost_opening_meta (
			id         INTEGER PRIMARY KEY CHECK (id = 1),
			day        TEXT    NOT NULL,
			stale      INTEGER NOT NULL,
			generation INTEGER NOT NULL
		);
		INSERT INTO cost_opening_meta (id, day, stale, generation) VALUES (1, '', 0, 0);
		CREATE TABLE cost_opening_convs (
			walk_key TEXT PRIMARY KEY,
			state    TEXT NOT NULL
		) WITHOUT ROWID;
		CREATE TABLE cost_opening_sessions (
			session_id TEXT PRIMARY KEY,
			state      TEXT NOT NULL
		) WITHOUT ROWID;
		CREATE TABLE cost_opening_spend (
			conv_id        TEXT PRIMARY KEY,
			first_real_day TEXT NOT NULL,
			first_any_day  TEXT NOT NULL
		) WITHOUT ROWID;`); err != nil {
		return err
	}
	// Some partial test schemas never had the table.
	var haveCostTable int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'session_cost_daily'`).Scan(&haveCostTable); err != nil {
		return err
	}
	if haveCostTable > 0 {
		if _, err := tx.Exec(`
			CREATE TRIGGER cost_opening_stale_insert AFTER INSERT ON session_cost_daily
			WHEN NEW.day < (SELECT day FROM cost_opening_meta)
			BEGIN UPDATE cost_opening_meta SET stale = 1; END;
			CREATE TRIGGER cost_opening_stale_delete AFTER DELETE ON session_cost_daily
			WHEN OLD.day < (SELECT day FROM cost_opening_meta)
			BEGIN UPDATE cost_opening_meta SET stale = 1; END;
			CREATE TRIGGER cost_opening_stale_update AFTER UPDATE ON session_cost_daily
			WHEN (OLD.day < (SELECT day FROM cost_opening_meta) OR NEW.day < (SELECT day FROM cost_opening_meta))
			 AND (OLD.session_id IS NOT NEW.session_id OR OLD.conv_id IS NOT NEW.conv_id OR OLD.day IS NOT NEW.day
			  OR OLD.cost_usd IS NOT NEW.cost_usd OR OLD.virtual_cost_usd IS NOT NEW.virtual_cost_usd
			  OR OLD.updated_at IS NOT NEW.updated_at OR OLD.model IS NOT NEW.model OR OLD.harness IS NOT NEW.harness)
			BEGIN UPDATE cost_opening_meta SET stale = 1; END;`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 294`); err != nil {
		return err
	}
	return tx.Commit()
}
