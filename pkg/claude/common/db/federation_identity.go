package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type FederationIdentityRotation struct {
	Statement   proto.Rotation `json:"statement"`
	State       string         `json:"state"`
	ReceivedAt  time.Time      `json:"received_at"`
	AcceptAfter time.Time      `json:"accept_after"`
	Reason      string         `json:"reason,omitempty"`
}

// ObserveFederationRotation starts a local detection window once. Retries do
// not shorten it; competing successors permanently require operator recovery.
func ObserveFederationRotation(r proto.Rotation, now time.Time, delay time.Duration) (bool, error) {
	if err := r.Verify(); err != nil {
		return false, err
	}
	if r.IssuedAt.After(now.Add(5 * time.Minute)) {
		return false, errors.New("rotation statement outside acceptance window")
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var key []byte
	if err = tx.QueryRow(`SELECT pubkey FROM federation_peers WHERE instance_id=?`, r.OldID).Scan(&key); err != nil {
		return false, err
	}
	if string(key) != string(r.OldKey) {
		return false, errors.New("rotation root is not the pinned key")
	}
	var previous, state string
	err = tx.QueryRow(`SELECT statement,state FROM federation_identity_rotations WHERE old_instance=?`, r.OldID).Scan(&previous, &state)
	if err == nil {
		var old proto.Rotation
		if err = json.Unmarshal([]byte(previous), &old); err != nil {
			return false, err
		}
		incoming, _ := json.Marshal(r)
		if previous == string(incoming) {
			return false, nil
		}
		if state == "pending" {
			_, err = tx.Exec(`UPDATE federation_identity_rotations SET state='conflict',reason='competing signed successors; explicit recovery required' WHERE old_instance=?`, r.OldID)
			if err != nil {
				return false, err
			}
			return true, tx.Commit()
		}
		return false, errors.New("predecessor already superseded, conflicted or revoked")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	after := now.Add(delay)
	if r.ActivateAt.After(after) {
		after = r.ActivateAt
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(`INSERT INTO federation_identity_rotations(old_instance,new_instance,statement,state,received_at,accept_after) VALUES(?,?,?,'pending',?,?)`, r.OldID, r.NewID, string(raw), dbTime(now), dbTime(after))
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func ListFederationIdentityRotations() ([]FederationIdentityRotation, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT statement,state,received_at,accept_after,reason FROM federation_identity_rotations ORDER BY received_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FederationIdentityRotation{}
	for rows.Next() {
		var r FederationIdentityRotation
		var raw string
		var received, after dbTimestamp
		if err = rows.Scan(&raw, &r.State, &received, &after, &r.Reason); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &r.Statement); err != nil {
			return nil, err
		}
		r.ReceivedAt, r.AcceptAfter = received.Time(), after.Time()
		out = append(out, r)
	}
	return out, rows.Err()
}

// AcceptFederationRotation rechecks both local state and pinned predecessor
// in the transaction that transfers authority. The certificate is public.
func AcceptFederationRotation(old string, now time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var raw, state string
	var after dbTimestamp
	if err = tx.QueryRow(`SELECT statement,state,accept_after FROM federation_identity_rotations WHERE old_instance=?`, old).Scan(&raw, &state, &after); err != nil {
		return err
	}
	if state == "accepted" {
		return nil
	}
	if state != "pending" || now.Before(after.Time()) {
		return errors.New("identity rotation is not ready for acceptance")
	}
	var r proto.Rotation
	if err = json.Unmarshal([]byte(raw), &r); err != nil {
		return err
	}
	if err = r.Verify(); err != nil {
		return err
	}
	if err = rebindFederationIdentityTx(tx, r.OldID, r.NewID, r.NewKey, r.OldKey, now); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE federation_identity_rotations SET state='accepted' WHERE old_instance=? AND state='pending'`, old); err != nil {
		return err
	}
	return tx.Commit()
}

// RebindFederationIdentity is the explicit recovery entry point. Caller must
// verify the replacement fingerprint and obtain operator consent first.
func RebindFederationIdentity(old, next string, key []byte, now time.Time) error {
	if len(key) != 32 || proto.InstanceID(key) != next || old == next {
		return errors.New("invalid recovery successor")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = rebindFederationIdentityTx(tx, old, next, key, nil, now); err != nil {
		return err
	}
	// Recovery evidence has no old-key signature and is never advertised as a
	// certificate. A public placeholder allows the same tombstone view.
	r := proto.Rotation{Version: 1, OldID: old, NewID: next, NewKey: key}
	raw, _ := json.Marshal(r)
	_, err = tx.Exec(`INSERT INTO federation_identity_rotations(old_instance,new_instance,statement,state,received_at,accept_after,reason) VALUES(?,?,?,'recovered',?,?,'explicit operator recovery') ON CONFLICT(old_instance) DO UPDATE SET state='recovered',new_instance=excluded.new_instance,statement=excluded.statement,reason='explicit operator recovery'`, old, next, string(raw), dbTime(now), dbTime(now))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func rebindFederationIdentityTx(tx *sql.Tx, old, next string, newKey, pinnedKey []byte, now time.Time) error {
	var p FederationPeer
	if err := tx.QueryRow(`SELECT pubkey,label,name,trust_level FROM federation_peers WHERE instance_id=?`, old).Scan(&p.PubKey, &p.Label, &p.Name, &p.TrustLevel); err != nil {
		return err
	}
	if pinnedKey != nil && string(p.PubKey) != string(pinnedKey) {
		return errors.New("rotation predecessor key changed")
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM federation_peers WHERE instance_id=?`, next).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return errors.New("successor is already trusted; refusing to merge authority")
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM federation_identity_rotations WHERE old_instance=?`, next).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return errors.New("successor is a retired or pending predecessor")
	}
	// Clear the unique label only inside this transaction, before inserting the
	// successor so foreign-key children can be moved without cascaded deletion.
	if _, err := tx.Exec(`UPDATE federation_peers SET label='' WHERE instance_id=?`, old); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO federation_peers(instance_id,pubkey,label,name,trusted_at,trust_level) SELECT ?,?,?,name,trusted_at,trust_level FROM federation_peers WHERE instance_id=?`, next, newKey, p.Label, old); err != nil {
		return err
	}
	for _, r := range FederationIdentityColumns {
		if r.Rule != IdentityRebind || r.Column != "peer" {
			continue
		}
		q := fmt.Sprintf(`UPDATE "%s" SET "%s"=? WHERE "%s"=?`, r.Table, r.Column, r.Column)
		if r.Table == "federation_agent_moves" || r.Table == "federation_teleports" {
			offer := "offer"
			if r.Table == "federation_agent_moves" {
				offer = "id"
			}
			q += ` AND (`
			if r.Table == "federation_agent_moves" {
				q += `state IN ('moved','retired') OR `
			}
			q += `EXISTS (SELECT 1 FROM federation_teleport_leases l WHERE l.peer="` + r.Table + `".peer AND l.direction="` + r.Table + `".direction AND l.offer="` + r.Table + `"."` + offer + `" AND l.state NOT IN ('recovered','released','superseded','clone')))`
		}
		if _, err := tx.Exec(q, next, old); err != nil {
			return err
		}
	}
	// These tables read typed JSON as their source of truth, not the indexed
	// peer column. Keep both representations in the same transaction.
	for _, table := range []string{"federation_teleport_leases", "federation_teleports"} {
		if _, err := tx.Exec(`UPDATE `+table+` SET snapshot=json_set(snapshot,'$.peer',?) WHERE peer=?`, next, next); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE federation_agent_moves SET payload=json_set(payload,'$.peer',?) WHERE peer=?`, next, next); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE federation_agent_moves SET payload=json_set(payload,'$.moved_to.instance',?) WHERE peer=? AND json_extract(payload,'$.moved_to.instance')=?`, next, next, old); err != nil {
		return err
	}
	if err := rebindIdentityScopesTx(tx, old, next); err != nil {
		return err
	}
	if err := closeIdentityCapabilitiesTx(tx, old, now); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM federation_peers WHERE instance_id=?`, old)
	return err
}

func rebindIdentityScopesTx(tx *sql.Tx, old, next string) error {
	for _, r := range FederationIdentityColumns {
		if r.Rule != IdentityRebind || (r.Column != "scope_json" && r.Column != "owner_scopes_json") {
			continue
		}
		rows, err := tx.Query(`SELECT rowid,"` + r.Column + `" FROM "` + r.Table + `" WHERE "` + r.Column + `"<>''`)
		if err != nil {
			return err
		}
		type edit struct {
			id  int64
			raw string
		}
		edits := []edit{}
		for rows.Next() {
			var id int64
			var raw string
			if err = rows.Scan(&id, &raw); err != nil {
				_ = rows.Close()
				return err
			}
			var scope map[string]json.RawMessage
			if err = json.Unmarshal([]byte(raw), &scope); err != nil {
				_ = rows.Close()
				return err
			}
			changed := false
			replace := func(dim map[string]json.RawMessage) error {
				value, ok := dim["peer"]
				if !ok {
					return nil
				}
				var peers []string
				if e := json.Unmarshal(value, &peers); e != nil {
					return e
				}
				localChange := false
				for i, v := range peers {
					if v == old {
						peers[i] = next
						localChange = true
					} else if len(v) > len(old) && v[:len(old)+1] == old+"/" {
						peers[i] = next + v[len(old):]
						localChange = true
					}
				}
				if localChange {
					dim["peer"], _ = json.Marshal(peers)
					changed = true
				}
				return nil
			}
			if r.Column == "owner_scopes_json" {
				for slug, value := range scope {
					var dim map[string]json.RawMessage
					if e := json.Unmarshal(value, &dim); e != nil {
						_ = rows.Close()
						return e
					}
					if e := replace(dim); e != nil {
						_ = rows.Close()
						return e
					}
					scope[slug], _ = json.Marshal(dim)
				}
			} else if e := replace(scope); e != nil {
				_ = rows.Close()
				return e
			}
			if changed {
				b, _ := json.Marshal(scope)
				edits = append(edits, edit{id, string(b)})
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		for _, e := range edits {
			if _, err = tx.Exec(`UPDATE "`+r.Table+`" SET "`+r.Column+`"=? WHERE rowid=?`, e.raw, e.id); err != nil {
				return err
			}
		}
	}
	return nil
}

func closeIdentityCapabilitiesTx(tx *sql.Tx, old string, now time.Time) error {
	// Live execution reservations deliberately remain charged until existing
	// lifecycle reconciliation proves the worker ended. No capacity is freed
	// merely because a peer's key changed.
	queries := []struct {
		q    string
		args []any
	}{
		{`UPDATE agent_routes SET state='withdrawn',withdraw_reason='peer identity changed',withdrawn_at=? WHERE id IN (SELECT route_id FROM federation_route_mirrors WHERE peer=?) AND state='ready'`, []any{dbTime(now), old}},
		{`UPDATE agent_route_leases SET state='closed',closed_at=? WHERE id IN (SELECT lease_id FROM federation_route_proxies WHERE peer=?) AND state='open'`, []any{dbTime(now), old}},
		{`UPDATE federation_jobs SET state='refused',result=json_set(result,'$.state','refused','$.code','peer_identity_changed','$.exit_code',1) WHERE peer=? AND direction='in' AND state='pending'`, []any{old}},
		{`UPDATE federation_jobs SET state='unknown',result=json_set(result,'$.state','unknown','$.code','peer_identity_changed','$.exit_code',1) WHERE peer=? AND direction='out' AND state IN ('pending','preparing','running','stopping')`, []any{old}},

		{`DELETE FROM federation_catalogs WHERE peer=?`, []any{old}},
		{`UPDATE federation_outbox SET state='refused',last_error='peer identity changed; resend under successor',updated_at=? WHERE to_instance=? AND state IN ('queued','sent')`, []any{dbTime(now), old}},
		{`UPDATE federation_spawn_requests SET status='rejected',reason='peer identity changed',decided_at=? WHERE from_instance=? AND status='pending'`, []any{dbTime(now), old}},
		{`UPDATE federation_bundle_offers SET state='declined',last_error='peer identity changed' WHERE peer=? AND state IN ('pending','ready')`, []any{old}},
		{`UPDATE federation_agent_moves SET state='blocked' WHERE peer=? AND state IN ('awaiting_confirmation','confirmed','retiring')`, []any{old}},
		{`UPDATE model_proxy_leases SET revoked=1 WHERE peer=?`, []any{old}},
		{`UPDATE model_proxy_launches SET revoked=1 WHERE reference LIKE ?`, []any{"%@" + old}},
		{`UPDATE model_proxy_launches SET revoked=1 WHERE lease IN (SELECT lease FROM model_proxy_worker_leases WHERE gateway=?)`, []any{old}},
		{`UPDATE federation_enrollments SET retired=1 WHERE peer=?`, []any{old}},
		// Sudo is temporary approval and is intentionally never inherited.
		{`UPDATE agent_sudo_grants SET revoked_at=? WHERE revoked_at IS NULL AND scope_json<>'' AND EXISTS (SELECT 1 FROM json_each(scope_json,'$.peer') WHERE value=? OR value LIKE ?)`, []any{dbTime(now), old, old + "/%"}},
	}
	for _, q := range queries {
		if _, err := tx.Exec(q.q, q.args...); err != nil {
			return err
		}
	}
	return nil
}

// RevokeFederationIdentity blocks automatic succession and removes remaining
// authority. A retired predecessor never rolls an accepted successor back.
func RevokeFederationIdentity(instance string, now time.Time) error {
	if !proto.ValidInstanceID(instance) {
		return errors.New("invalid instance ID")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	r := proto.Rotation{Version: 1, OldID: instance}
	raw, _ := json.Marshal(r)
	if _, err = tx.Exec(`INSERT INTO federation_identity_rotations(old_instance,new_instance,statement,state,received_at,accept_after,reason) VALUES(?,'',?,'revoked',?,?,'operator revoked predecessor') ON CONFLICT(old_instance) DO UPDATE SET state=CASE WHEN state IN ('accepted','recovered') THEN state ELSE 'revoked' END,reason='operator revoked predecessor'`, instance, string(raw), dbTime(now), dbTime(now)); err != nil {
		return err
	}
	if err = closeIdentityCapabilitiesTx(tx, instance, now); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM federation_peers WHERE instance_id=?`, instance); err != nil {
		return err
	}
	return tx.Commit()
}

// RetireLocalFederationIdentity invalidates capabilities created under the
// local predecessor; durable worker reservations remain until reconciliation.
func RetireLocalFederationIdentity(now time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`UPDATE model_proxy_leases SET revoked=1`,
		`UPDATE model_proxy_launches SET revoked=1`,
		`UPDATE federation_enrollments SET retired=1`,
		`UPDATE federation_enroll_tokens SET revoked=1`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`UPDATE federation_outbox SET state='refused',last_error='local identity changed; resend explicitly',updated_at=? WHERE state IN ('queued','sent')`, dbTime(now)); err != nil {
		return err
	}
	return tx.Commit()
}

// ResolveFederationIdentitySuccessor is for explicit continuation routing (for
// example teleport --home), never automatic mail forwarding or fresh trust.
func ResolveFederationIdentitySuccessor(instance string) (string, error) {
	d, err := Open()
	if err != nil {
		return "", err
	}
	// The public proof chain is bounded for discovery, but explicit recovery
	// can extend already committed local history. Never return an intermediate
	// identity: policy and capacity checks must reach the current successor.
	seen := map[string]bool{}
	for {
		if seen[instance] {
			return "", errors.New("cyclic successor chain")
		}
		seen[instance] = true
		var next string
		err = d.QueryRow(`SELECT new_instance FROM federation_identity_rotations WHERE old_instance=? AND state IN ('accepted','recovered')`, instance).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			return instance, nil
		}
		if err != nil {
			return "", err
		}
		if !proto.ValidInstanceID(next) || next == instance {
			return "", errors.New("invalid successor chain")
		}
		instance = next
	}
}
