package db

import (
	"database/sql"
	"fmt"
)

// mailboxTallyTriggers lists what the Messages roster's tallies (MailboxCounts,
// MailboxRosterTotals) read. Each row's change bumps
// mailbox_tally_generation.gen, so the tallies can be reused until something
// they depend on really changed. Columns the tallies never read, such as the
// nudge bookkeeping that churns on every delivery, stay out of the update arm.
var mailboxTallyTriggers = []struct{ table, columns string }{
	{"agent_messages", "group_id, from_conv, to_conv, read_at, created_at, subject"},
	{"agents", "current_conv_id, retired_at"},
	{"agent_group_members", "group_id, agent_id"},
	{"agent_federation_presence", "agent_id, state"},
}

// migrateV292toV293 adds the generation the Messages roster tallies are
// memoized against. Those tallies scan all of agent_messages, which is never
// pruned, on every 2s roster refresh. A TTL would make unread badges lag and
// MAX(id) misses reads and deletes; triggers see every writer, including the
// other tclaude processes sharing this database.
func migrateV292toV293(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE mailbox_tally_generation (
		id  INTEGER PRIMARY KEY CHECK (id = 1),
		gen INTEGER NOT NULL
	) STRICT;
	INSERT INTO mailbox_tally_generation (id, gen) VALUES (1, 0);`); err != nil {
		return fmt.Errorf("migrate v292→v293 (create generation): %w", err)
	}
	const bump = `BEGIN UPDATE mailbox_tally_generation SET gen = gen + 1; END`
	for _, t := range mailboxTallyTriggers {
		// Some partial test schemas never had the table.
		found, err := txTableExists(tx, t.table)
		if err != nil {
			return fmt.Errorf("migrate v292→v293 (probe %s): %w", t.table, err)
		}
		if !found {
			continue
		}
		for _, stmt := range []string{
			`CREATE TRIGGER mailbox_tally_` + t.table + `_insert AFTER INSERT ON ` + t.table + ` ` + bump,
			`CREATE TRIGGER mailbox_tally_` + t.table + `_delete AFTER DELETE ON ` + t.table + ` ` + bump,
			`CREATE TRIGGER mailbox_tally_` + t.table + `_update AFTER UPDATE OF ` + t.columns + ` ON ` + t.table + ` ` + bump,
		} {
			if _, err := tx.Exec(stmt); err != nil {
				return fmt.Errorf("migrate v292→v293 (trigger on %s): %w", t.table, err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 293`); err != nil {
		return err
	}
	return tx.Commit()
}
