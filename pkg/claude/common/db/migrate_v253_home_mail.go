package db

import "database/sql"

func migrateV252toV253(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`
CREATE TABLE federation_agent_locations (
 agent_id TEXT PRIMARY KEY, home_instance TEXT NOT NULL, current_instance TEXT NOT NULL,
 epoch TEXT NOT NULL, hop_count INTEGER NOT NULL, arrival_offer TEXT NOT NULL, updated_at INTEGER NOT NULL
) STRICT;
CREATE TABLE federation_mail_custody (
 sender_instance TEXT NOT NULL, envelope_id TEXT NOT NULL, agent_id TEXT NOT NULL,
 ingress_instance TEXT NOT NULL, ingress_envelope TEXT NOT NULL, payload TEXT NOT NULL,
 state TEXT NOT NULL, destination TEXT NOT NULL DEFAULT '', attempt_id TEXT NOT NULL DEFAULT '',
 next_attempt_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 PRIMARY KEY(sender_instance,envelope_id,agent_id)
) STRICT;
CREATE TABLE federation_agent_mail_deliveries (
 agent_id TEXT NOT NULL, sender_instance TEXT NOT NULL, envelope_id TEXT NOT NULL, expires_at INTEGER NOT NULL,
 PRIMARY KEY(agent_id,sender_instance,envelope_id)
) STRICT;
CREATE TABLE federation_agent_mail_fences (
 agent_id TEXT PRIMARY KEY, token TEXT NOT NULL, offer TEXT NOT NULL DEFAULT '', expires_at INTEGER NOT NULL
) STRICT; UPDATE schema_version SET version=253;`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
