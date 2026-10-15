package db

import "database/sql"

// migrateV264toV265 adds the library and group side of process templates
// (tcl-8o4rjj). Both are tclaude-side metadata, never template content: a
// template's source and semantic hash are unchanged by being pinned to a group
// or listed in the library, and an exported template never carries a group.
//
//   - process_template_listing records a template's library listing. A
//     template with no row is listed, which is every template saved before
//     this existed.
//   - process_group_pins is a group's ordered list of saved processes. Pins
//     cascade with the group; a pin to a deleted template is removed with it.
func migrateV264toV265(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS process_template_listing (
	template_id TEXT PRIMARY KEY CHECK(length(CAST(template_id AS BLOB)) <= 256),
	listed      INTEGER NOT NULL CHECK(listed IN (0, 1)),
	updated_at  INTEGER NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS process_group_pins (
	group_id    INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
	template_id TEXT NOT NULL CHECK(length(CAST(template_id AS BLOB)) <= 256),
	position    INTEGER NOT NULL,
	pinned_at   INTEGER NOT NULL,
	PRIMARY KEY (group_id, template_id)
) STRICT;
CREATE INDEX IF NOT EXISTS idx_process_group_pins_template ON process_group_pins(template_id);
UPDATE schema_version SET version = 265;`); err != nil {
		return err
	}
	return tx.Commit()
}
