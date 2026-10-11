package db

import "database/sql"

func migrateV253toV254(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`
CREATE TABLE federation_group_identities (
 group_id INTEGER PRIMARY KEY REFERENCES agent_groups(id) ON DELETE CASCADE,
 opaque_id TEXT NOT NULL UNIQUE
) STRICT;
CREATE TABLE federation_peer_proposals (
 id TEXT PRIMARY KEY, peer TEXT NOT NULL, origin_instance TEXT NOT NULL,
 revision INTEGER NOT NULL, body TEXT NOT NULL, decision TEXT NOT NULL DEFAULT '{}', authority TEXT NOT NULL DEFAULT '{}', state TEXT NOT NULL,
 created_at INTEGER NOT NULL, deadline_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
) STRICT;
CREATE TABLE federation_group_links (
 id TEXT PRIMARY KEY, local_group_id INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
 peer TEXT NOT NULL, remote_group_id TEXT NOT NULL, remote_group_name TEXT NOT NULL,
 direction TEXT NOT NULL CHECK(direction IN ('in','out')),
 mode TEXT NOT NULL CHECK(mode IN ('members->members','owners->members')),
 local_state TEXT NOT NULL, remote_state TEXT NOT NULL, proposal_id TEXT NOT NULL,
 item_id TEXT NOT NULL, remote_state_at INTEGER, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 UNIQUE(proposal_id,item_id)
) STRICT;
CREATE INDEX federation_group_links_lookup ON federation_group_links(peer,remote_group_id,local_group_id,direction);
CREATE TABLE federation_proposal_item_decisions (
 proposal_id TEXT NOT NULL REFERENCES federation_peer_proposals(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL, item_id TEXT NOT NULL, accepted INTEGER NOT NULL,
 applied_here INTEGER NOT NULL DEFAULT 0, applied_remote INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(proposal_id,revision,item_id)
) STRICT;
CREATE TABLE federation_proposal_events (
 id TEXT PRIMARY KEY, proposal_id TEXT NOT NULL, peer TEXT NOT NULL,
 kind TEXT NOT NULL, payload TEXT NOT NULL, expires_at INTEGER NOT NULL, queued INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE TABLE federation_link_mail (
 envelope_id TEXT PRIMARY KEY REFERENCES federation_outbox(envelope_id) ON DELETE CASCADE,
 link_id TEXT NOT NULL
) STRICT;
UPDATE schema_version SET version=254;`)
	if err != nil {
		return err
	}
	// Historical recovery fixtures can omit the group table. As in earlier
	// group migrations, seed identities only when that source table exists.
	var haveGroups int
	if err = tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='agent_groups'`).Scan(&haveGroups); err != nil {
		return err
	}
	if haveGroups != 0 {
		if _, err = tx.Exec(`INSERT INTO federation_group_identities(group_id,opaque_id) SELECT id,'grp_'||lower(hex(randomblob(16))) FROM agent_groups`); err != nil {
			return err
		}
		if _, err = tx.Exec(`CREATE TRIGGER agent_groups_federation_identity AFTER INSERT ON agent_groups
 BEGIN INSERT INTO federation_group_identities(group_id,opaque_id) VALUES(NEW.id,'grp_'||lower(hex(randomblob(16)))); END`); err != nil {
			return err
		}

	}
	return tx.Commit()
}
