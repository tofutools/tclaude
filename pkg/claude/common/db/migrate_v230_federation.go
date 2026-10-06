package db

import (
	"database/sql"
	"fmt"
)

// migrateV229toV230 adds the federation tables (epic tcl-ozzhre): trusted
// peer instances, group exports and imports, cached remote catalogs, the
// durable outbound envelope queue, and the side table that marks
// agent_messages rows received from a remote instance.
//
// federation_seen is the replay guard: one row per accepted mail envelope,
// keyed by sender, kept until the envelope expires and deliberately not tied
// to the message row, so deleting or pruning the message cannot reopen the
// envelope for replay.
//
// The inbound marker is a side table (like operator_agent_messages) rather
// than new agent_messages columns, so agentMessageColumns and every scan
// stay untouched. All statements are IF NOT EXISTS so a half-applied run
// converges on re-run.
func migrateV229toV230(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v229→v230 (federation): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(federationSchema); err != nil {
		return fmt.Errorf("migrate v229→v230 (federation): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 230`); err != nil {
		return fmt.Errorf("migrate v229→v230 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v229→v230 (commit): %w", err)
	}
	return nil
}

const federationSchema = `
CREATE TABLE IF NOT EXISTS federation_peers (
	instance_id TEXT PRIMARY KEY,
	pubkey      BLOB NOT NULL,
	label       TEXT NOT NULL DEFAULT '',
	name        TEXT NOT NULL DEFAULT '',
	trusted_at  INTEGER NOT NULL
) STRICT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_federation_peers_label
	ON federation_peers(label) WHERE label != '';

CREATE TABLE IF NOT EXISTS federation_catalogs (
	peer        TEXT PRIMARY KEY,
	payload     TEXT NOT NULL,
	received_at INTEGER NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS federation_outbox (
	envelope_id     TEXT PRIMARY KEY,
	kind            TEXT NOT NULL,
	to_instance     TEXT NOT NULL,
	to_agent        TEXT NOT NULL DEFAULT '',
	to_label        TEXT NOT NULL DEFAULT '',
	from_conv       TEXT NOT NULL DEFAULT '',
	from_agent      TEXT NOT NULL DEFAULT '',
	in_reply_to     TEXT NOT NULL DEFAULT '',
	subject         TEXT NOT NULL DEFAULT '',
	body_preview    TEXT NOT NULL DEFAULT '',
	sealed          BLOB NOT NULL,
	state           TEXT NOT NULL,
	attempts        INTEGER NOT NULL DEFAULT 0,
	next_attempt_at INTEGER NOT NULL,
	last_error      TEXT NOT NULL DEFAULT '',
	created_at      INTEGER NOT NULL,
	expires_at      INTEGER NOT NULL,
	updated_at      INTEGER NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS idx_federation_outbox_state
	ON federation_outbox(state, next_attempt_at);

CREATE TABLE IF NOT EXISTS federation_inbound (
	message_id    INTEGER PRIMARY KEY REFERENCES agent_messages(id) ON DELETE CASCADE,
	envelope_id   TEXT NOT NULL,
	from_instance TEXT NOT NULL,
	from_agent    TEXT NOT NULL DEFAULT '',
	from_name     TEXT NOT NULL DEFAULT '',
	received_at   INTEGER NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS idx_federation_inbound_envelope
	ON federation_inbound(from_instance, envelope_id);

CREATE TABLE IF NOT EXISTS federation_seen (
	from_instance TEXT NOT NULL,
	envelope_id   TEXT NOT NULL,
	expires_at    INTEGER NOT NULL,
	PRIMARY KEY (from_instance, envelope_id)
) STRICT;
`
