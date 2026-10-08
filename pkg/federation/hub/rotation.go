package hub

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// ObserveRotations admits a successor only after a locally observed window and
// an authenticated hello from that successor. Neither names nor hub assertions
// establish key continuity.
func (s *Store) ObserveRotations(chain []proto.Rotation, authenticated string, now time.Time, window time.Duration) error {
	if len(chain) == 0 {
		return nil
	}
	if err := proto.VerifyRotationChain(chain, chain[0].OldID, chain[0].OldKey, chain[len(chain)-1].NewID); err != nil {
		return err
	}
	for _, r := range chain {
		proof := authenticated
		if authenticated == chain[len(chain)-1].NewID {
			proof = r.NewID
		}
		if err := s.observeRotation(r, proof, now, window); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) observeRotation(r proto.Rotation, authenticated string, now time.Time, window time.Duration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	var previous, state, after, recordedNext string
	err = tx.QueryRow(`SELECT statement,state,accept_after,new_instance FROM identity_rotations WHERE old_instance=?`, r.OldID).Scan(&previous, &state, &after, &recordedNext)
	if err == nil {
		if state == "recovered" {
			if recordedNext == r.NewID {
				return nil
			}
			return errors.New("predecessor was explicitly recovered to a different identity")
		}
		if state == "accepted" {
			if previous == string(raw) {
				return nil
			}
			return errors.New("predecessor already has an accepted successor")
		}
		if state == "revoked" || state == "conflict" {
			return errors.New("rotation predecessor revoked or conflicted; hub admin recovery required")
		}
		if previous != string(raw) {
			_, err = tx.Exec(`UPDATE identity_rotations SET state='conflict' WHERE old_instance=?`, r.OldID)
			if err != nil {
				return err
			}
			if err = tx.Commit(); err != nil {
				return err
			}
			return errors.New("competing signed rotation successors; hub admin recovery required")
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var revoked bool
	var key []byte
	if err = tx.QueryRow(`SELECT revoked,pubkey FROM instances WHERE instance_id=?`, r.OldID).Scan(&revoked, &key); err != nil {
		return fmt.Errorf("rotation predecessor not admitted: %w", err)
	}
	if revoked || len(key) > 0 && string(key) != string(r.OldKey) {
		return errors.New("rotation predecessor revoked or key changed")
	}
	if previous == "" {
		if r.IssuedAt.After(now.Add(5 * time.Minute)) {
			return errors.New("rotation statement outside admission window")
		}
		activate := now.Add(window)
		if r.ActivateAt.After(activate) {
			activate = r.ActivateAt
		}
		after = ts(activate)
		if _, err = tx.Exec(`INSERT INTO identity_rotations(old_instance,new_instance,statement,state,received_at,accept_after) VALUES(?,?,?,'pending',?,?)`, r.OldID, r.NewID, string(raw), ts(now), after); err != nil {
			return err
		}
	}
	if authenticated != r.NewID || now.Before(parseTS(after)) {
		return tx.Commit()
	}
	var exists int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM instances WHERE instance_id=?`, r.NewID).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return errors.New("rotation successor already admitted or revoked; refusing to merge admission")
	}
	if err = tx.QueryRow(`SELECT COUNT(*) FROM identity_rotations WHERE old_instance=?`, r.NewID).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return errors.New("rotation successor was superseded or revoked")
	}
	if _, err = tx.Exec(`INSERT INTO instances(instance_id,pubkey,name,version,admitted_at,last_seen) SELECT ?,?,name,version,admitted_at,? FROM instances WHERE instance_id=?`, r.NewID, r.NewKey, ts(now), r.OldID); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO instance_spaces(instance_id,space) SELECT ?,space FROM instance_spaces WHERE instance_id=?`, r.NewID, r.OldID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE instances SET revoked=1 WHERE instance_id=?`, r.OldID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE identity_rotations SET state='accepted' WHERE old_instance=?`, r.OldID); err != nil {
		return err
	}
	return tx.Commit()
}

// RotationChain returns only public evidence. Pending evidence is published on
// its predecessor so peers can begin their own independent detection window.
func (s *Store) RotationChain(instance string) ([]proto.Rotation, error) {
	rows, err := s.db.Query(`SELECT statement FROM identity_rotations WHERE state IN ('pending','accepted') ORDER BY received_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := []proto.Rotation{}
	for rows.Next() {
		var raw string
		var r proto.Rotation
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &r); err != nil {
			return nil, err
		}
		all = append(all, r)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	chain := []proto.Rotation{}
	current := instance
	// Prefer the candidate rooted at a still-current predecessor, then walk back
	// through its accepted ancestry. A branch never appears as accepted evidence.
	for _, r := range all {
		if r.OldID == instance {
			chain = append(chain, r)
			break
		}
	}
	for len(chain) < proto.MaxRotationHops {
		found := false
		for _, r := range all {
			if r.NewID == current {
				chain = append([]proto.Rotation{r}, chain...)
				current = r.OldID
				found = true
				break
			}
		}
		if !found {
			break
		}
	}
	return chain, nil
}

func (s *Store) IdentityRetired(instance string) (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM identity_rotations WHERE old_instance=? AND state IN ('accepted','conflict','revoked','recovered')`, instance).Scan(&count)
	return count > 0, err
}

// RecoverIdentity is an operator-only admission rebind, never a certificate.
// The replacement proves its own key at hello; its ID is checked out of band.
func (s *Store) RecoverIdentity(old, next string, now time.Time) error {
	if !proto.ValidInstanceID(old) || !proto.ValidInstanceID(next) || old == next {
		return errors.New("invalid recovery identity IDs")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM instances WHERE instance_id=?`, old).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return errors.New("old identity has no admission to recover")
	}
	var retired int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM identity_rotations WHERE old_instance=?`, next).Scan(&retired); err != nil {
		return err
	}
	if retired != 0 {
		return errors.New("replacement has already been retired or has a pending transition")
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO instances(instance_id,admitted_at) SELECT ?,admitted_at FROM instances WHERE instance_id=?`, next, old); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE instances SET revoked=0 WHERE instance_id=?`, next); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM instance_spaces WHERE instance_id=?`, next); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO instance_spaces(instance_id,space) SELECT ?,space FROM instance_spaces WHERE instance_id=?`, next, old); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE instances SET revoked=1 WHERE instance_id=?`, old); err != nil {
		return err
	}
	r := proto.Rotation{Version: 1, OldID: old, NewID: next}
	raw, _ := json.Marshal(r)
	if _, err = tx.Exec(`INSERT INTO identity_rotations(old_instance,new_instance,statement,state,received_at,accept_after) VALUES(?,?,?,'recovered',?,?) ON CONFLICT(old_instance) DO UPDATE SET new_instance=excluded.new_instance,statement=excluded.statement,state='recovered'`, old, next, string(raw), ts(now), ts(now)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RevokeOldIdentity(instance string, now time.Time) error {
	if !proto.ValidInstanceID(instance) {
		return errors.New("invalid instance ID")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	r := proto.Rotation{Version: 1, OldID: instance}
	raw, _ := json.Marshal(r)
	if _, err = tx.Exec(`INSERT INTO identity_rotations(old_instance,new_instance,statement,state,received_at,accept_after) VALUES(?,'',?,'revoked',?,?) ON CONFLICT(old_instance) DO UPDATE SET state=CASE WHEN state IN ('accepted','recovered') THEN state ELSE 'revoked' END`, instance, string(raw), ts(now), ts(now)); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE instances SET revoked=1 WHERE instance_id=?`, instance); err != nil {
		return err
	}
	return tx.Commit()
}
