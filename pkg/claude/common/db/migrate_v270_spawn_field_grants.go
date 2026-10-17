package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
)

// migrateV269toV270 turns a groups.members.spawn grant's allowed_overrides
// list (the launch settings a peer may choose on a remote spawn) into
// grants of their own: groups.members.spawn.cwd, .harness, .model and
// .effort, with the spawn grant's scope, on a peer or a pool. They take no
// expiry: a field grant counts only beside a spawn grant that is in force,
// so the spawn grant's own expiry (and its renewals) still bound them. The
// list is dropped from the stored policy.
func migrateV269toV270(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v269→v270 (spawn field grants): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	type row struct {
		owner           string
		scope, policy   string
		createdAt       int64
		expiresAt       sql.NullInt64
		fields          []string
		rewrittenPolicy string
	}
	collect := func(query string, withExpiry bool) ([]row, error) {
		rows, err := tx.Query(query)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		var out []row
		for rows.Next() {
			var r row
			if withExpiry {
				err = rows.Scan(&r.owner, &r.scope, &r.policy, &r.createdAt, &r.expiresAt)
			} else {
				err = rows.Scan(&r.owner, &r.scope, &r.policy, &r.createdAt)
			}
			if err != nil {
				return nil, err
			}
			var policy map[string]json.RawMessage
			if json.Unmarshal([]byte(r.policy), &policy) != nil || policy["allowed_overrides"] == nil {
				continue
			}
			var fields []string
			_ = json.Unmarshal(policy["allowed_overrides"], &fields)
			delete(policy, "allowed_overrides")
			raw, err := json.Marshal(policy)
			if err != nil {
				return nil, err
			}
			for _, f := range fields {
				if slices.Contains([]string{"cwd", "harness", "model", "effort"}, f) && !slices.Contains(r.fields, f) {
					r.fields = append(r.fields, f)
				}
			}
			r.rewrittenPolicy = string(raw)
			out = append(out, r)
		}
		return out, rows.Err()
	}
	peers, err := collect(`SELECT peer,scope,spawn_policy,created_at,expires_at FROM federation_peer_grants WHERE slug='groups.members.spawn'`, true)
	if err != nil {
		return fmt.Errorf("migrate v269→v270 (read peer grants): %w", err)
	}
	for _, r := range peers {
		for _, f := range r.fields {
			if _, err := tx.Exec(`INSERT INTO federation_peer_grants(peer,slug,scope,spawn_policy,created_at) VALUES(?,?,?,'{}',?) ON CONFLICT(peer,slug,scope) DO NOTHING`,
				r.owner, "groups.members.spawn."+f, r.scope, r.createdAt); err != nil {
				return fmt.Errorf("migrate v269→v270 (peer field grant): %w", err)
			}
		}
		if _, err := tx.Exec(`UPDATE federation_peer_grants SET spawn_policy=? WHERE peer=? AND slug='groups.members.spawn' AND scope=?`, r.rewrittenPolicy, r.owner, r.scope); err != nil {
			return fmt.Errorf("migrate v269→v270 (peer policy): %w", err)
		}
	}
	pools, err := collect(`SELECT group_id,scope,spawn_policy,created_at FROM federation_node_group_grants WHERE slug='groups.members.spawn'`, false)
	if err != nil {
		return fmt.Errorf("migrate v269→v270 (read pool grants): %w", err)
	}
	for _, r := range pools {
		for _, f := range r.fields {
			if _, err := tx.Exec(`INSERT INTO federation_node_group_grants(group_id,slug,scope,spawn_policy,created_at) VALUES(?,?,?,'{}',?) ON CONFLICT(group_id,slug,scope) DO NOTHING`,
				r.owner, "groups.members.spawn."+f, r.scope, r.createdAt); err != nil {
				return fmt.Errorf("migrate v269→v270 (pool field grant): %w", err)
			}
		}
		if _, err := tx.Exec(`UPDATE federation_node_group_grants SET spawn_policy=? WHERE group_id=? AND slug='groups.members.spawn' AND scope=?`, r.rewrittenPolicy, r.owner, r.scope); err != nil {
			return fmt.Errorf("migrate v269→v270 (pool policy): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 270`); err != nil {
		return fmt.Errorf("migrate v269→v270 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v269→v270 (commit): %w", err)
	}
	return nil
}
