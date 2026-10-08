package db

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Enrollment state contains only the signed public token and a secret hash.
// Completed bindings survive untrust as tombstones: a bearer cannot re-trust.
type FederationEnrollmentToken struct {
	ID        string    `json:"id"`
	Public    string    `json:"public_token"`
	MaxUses   int       `json:"max_uses"`
	Used      int       `json:"used"`
	Revoked   bool      `json:"revoked"`
	ExpiresAt time.Time `json:"expires_at"`
}
type FederationEnrollment struct {
	Direction string `json:"direction"`
	TokenID   string `json:"token_id"`
	Peer      string `json:"peer"`
	PeerKey   []byte `json:"peer_key"`
	LocalKey  []byte `json:"local_key"`
	Public    string `json:"public_token"`
	Retired   bool   `json:"retired"`
}

var ErrEnrollmentRefused = errors.New("enrollment refused: token expired, revoked, exhausted, changed, or peer already trusted/retired")

func SaveFederationEnrollmentToken(t *proto.EnrollmentToken, uses int) error {
	if uses < 1 || uses > 10000 {
		return ErrEnrollmentRefused
	}
	d, e := Open()
	if e != nil {
		return e
	}
	tx, e := d.Begin()
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	if _, e = tx.Exec(`UPDATE federation_node_profiles SET revision=revision WHERE id=?`, t.Claims.ProfileID); e != nil {
		return e
	}
	var rev int64
	if e = tx.QueryRow(`SELECT revision FROM federation_node_profiles WHERE id=?`, t.Claims.ProfileID).Scan(&rev); e != nil {
		return e
	}
	if rev != t.Claims.ProfileRevision {
		return ErrEnrollmentRefused
	}
	_, e = tx.Exec(`INSERT INTO federation_enroll_tokens(id,public_token,secret_hash,profile_id,profile_revision,max_uses,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?)`, t.Claims.TokenID, t.Public, t.Claims.SecretHash, t.Claims.ProfileID, rev, uses, dbTime(time.Now()), dbTime(t.Claims.ExpiresAt))
	if e != nil {
		return e
	}
	return tx.Commit()
}
func ListFederationEnrollmentTokens() ([]FederationEnrollmentToken, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	rows, e := d.Query(`SELECT id,public_token,max_uses,used_count,revoked,expires_at FROM federation_enroll_tokens ORDER BY created_at,id`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []FederationEnrollmentToken{}
	for rows.Next() {
		var t FederationEnrollmentToken
		var at dbTimestamp
		if e = rows.Scan(&t.ID, &t.Public, &t.MaxUses, &t.Used, &t.Revoked, &at); e != nil {
			return nil, e
		}
		t.ExpiresAt = at.Time()
		out = append(out, t)
	}
	return out, rows.Err()
}
func RevokeFederationEnrollmentToken(id string) error {
	d, e := Open()
	if e != nil {
		return e
	}
	r, e := d.Exec(`UPDATE federation_enroll_tokens SET revoked=1 WHERE id=?`, id)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e == nil && n != 1 {
		return sql.ErrNoRows
	}
	return e
}
func ListFederationEnrollments() ([]FederationEnrollment, error) {
	d, e := Open()
	if e != nil {
		return nil, e
	}
	rows, e := d.Query(`SELECT direction,token_id,peer,peer_key,local_key,public_token,retired FROM federation_enrollments ORDER BY created_at,token_id,peer`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []FederationEnrollment{}
	for rows.Next() {
		var v FederationEnrollment
		if e = rows.Scan(&v.Direction, &v.TokenID, &v.Peer, &v.PeerKey, &v.LocalKey, &v.Public, &v.Retired); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func enrollmentBinding(tx *sql.Tx, direction, token, peer string) (*FederationEnrollment, error) {
	var v FederationEnrollment
	v.Direction = direction
	v.TokenID = token
	v.Peer = peer
	e := tx.QueryRow(`SELECT peer_key,local_key,public_token,retired FROM federation_enrollments WHERE direction=? AND token_id=? AND peer=?`, direction, token, peer).Scan(&v.PeerKey, &v.LocalKey, &v.Public, &v.Retired)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	return &v, e
}
func insertEnrollment(tx *sql.Tx, direction string, t *proto.EnrollmentToken, peer, local []byte) error {
	_, e := tx.Exec(`INSERT INTO federation_enrollments(direction,token_id,peer,peer_key,local_key,public_token,created_at) VALUES(?,?,?,?,?,?,?)`, direction, t.Claims.TokenID, proto.InstanceID(peer), peer, local, t.Public, dbTime(time.Now()))
	return e
}

// RedeemFederationEnrollment serializes first use, profile application and the
// receipt in one writer transaction. Replays return the receipt, never reapply.
func RedeemFederationEnrollment(t *proto.EnrollmentToken, nodeKey, localKey []byte) (bool, error) {
	if !bytes.Equal(t.Claims.MasterKey, localKey) {
		return false, ErrEnrollmentRefused
	}
	d, e := Open()
	if e != nil {
		return false, e
	}
	tx, e := d.Begin()
	if e != nil {
		return false, e
	}
	defer func() { _ = tx.Rollback() }()
	if _, e = tx.Exec(`UPDATE federation_enroll_tokens SET used_count=used_count WHERE id=?`, t.Claims.TokenID); e != nil {
		return false, e
	}
	peer := proto.InstanceID(nodeKey)
	binding, e := enrollmentBinding(tx, "issuer", t.Claims.TokenID, peer)
	if e != nil {
		return false, e
	}
	var trusted int
	if e = tx.QueryRow(`SELECT count(*) FROM federation_peers WHERE instance_id=?`, peer).Scan(&trusted); e != nil {
		return false, e
	}
	if binding != nil {
		if binding.Retired || trusted != 1 || binding.Public != t.Public || !bytes.Equal(binding.LocalKey, localKey) || !bytes.Equal(binding.PeerKey, nodeKey) {
			return false, ErrEnrollmentRefused
		}
		return false, tx.Commit()
	}
	var public string
	var hash []byte
	var max, used, revoked int
	var expiry dbTimestamp
	if e = tx.QueryRow(`SELECT public_token,secret_hash,max_uses,used_count,revoked,expires_at FROM federation_enroll_tokens WHERE id=?`, t.Claims.TokenID).Scan(&public, &hash, &max, &used, &revoked, &expiry); e != nil {
		return false, ErrEnrollmentRefused
	}
	if public != t.Public || !bytes.Equal(hash, t.Claims.SecretHash) || revoked != 0 || used >= max || !time.Now().Before(expiry.Time()) || trusted != 0 {
		return false, ErrEnrollmentRefused
	}
	p, e := scanNodeProfile(tx.QueryRow(`SELECT id,name,revision,definition FROM federation_node_profiles WHERE id=?`, t.Claims.ProfileID))
	if e != nil {
		return false, ErrEnrollmentRefused
	}
	if p.Revision != t.Claims.ProfileRevision {
		return false, ErrEnrollmentRefused
	}
	np := &FederationPeer{InstanceID: peer, PubKey: nodeKey, Label: "node-" + peer, TrustLevel: p.Definition.TrustLevel}
	plan, e := planFederationNodeProfileTx(tx, p.ID, peer, "", np)
	if e != nil {
		return false, e
	}
	if _, e = planFederationNodeProfileTx(tx, p.ID, peer, plan.Token, np); e != nil {
		return false, e
	}
	if e = insertEnrollment(tx, "issuer", t, nodeKey, localKey); e != nil {
		return false, e
	}
	if _, e = tx.Exec(`UPDATE federation_enroll_tokens SET used_count=used_count+1 WHERE id=?`, t.Claims.TokenID); e != nil {
		return false, e
	}
	return true, tx.Commit()
}

// EnrollmentNodePreview binds consent to the local peer and receipt state.
func enrollmentNodePreviewTx(tx *sql.Tx, t *proto.EnrollmentToken) (string, error) {
	var raw []any
	var key []byte
	var label, name, level string
	var at int64
	e := tx.QueryRow(`SELECT pubkey,label,name,trust_level,trusted_at FROM federation_peers WHERE instance_id=?`, t.Claims.Master).Scan(&key, &label, &name, &level, &at)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return "", e
	}
	raw = append(raw, t.Public, key, label, name, level, at)
	b, e := enrollmentBinding(tx, "node", t.Claims.TokenID, t.Claims.Master)
	if e != nil {
		return "", e
	}
	if (b == nil && len(key) != 0) || (b != nil && (b.Retired || len(key) == 0 || b.Public != t.Public || !bytes.Equal(b.PeerKey, t.Claims.MasterKey))) {
		return "", ErrEnrollmentRefused
	}
	raw = append(raw, b)
	v, e := json.Marshal(raw)
	if e != nil {
		return "", e
	}
	sum := sha256.Sum256(v)
	return hex.EncodeToString(sum[:]), nil
}
func FederationEnrollmentNodePreview(t *proto.EnrollmentToken) (string, error) {
	d, e := Open()
	if e != nil {
		return "", e
	}
	tx, e := d.Begin()
	if e != nil {
		return "", e
	}
	defer func() { _ = tx.Rollback() }()
	return enrollmentNodePreviewTx(tx, t)
}
func CompleteFederationEnrollment(t *proto.EnrollmentToken, localKey []byte, preview string) error {
	d, e := Open()
	if e != nil {
		return e
	}
	tx, e := d.Begin()
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	// Acquire the SQLite writer before reading consent state, including first trust.
	if _, e = tx.Exec(`UPDATE federation_enrollments SET retired=retired WHERE token_id=?`, t.Claims.TokenID); e != nil {
		return e
	}
	current, e := enrollmentNodePreviewTx(tx, t)
	if e != nil {
		return e
	}
	if preview == "" || current != preview {
		return errors.New("local trust changed; preview enrollment again")
	}
	b, e := enrollmentBinding(tx, "node", t.Claims.TokenID, t.Claims.Master)
	if e != nil {
		return e
	}
	if b != nil {
		if b.Retired || b.Public != t.Public || !bytes.Equal(b.LocalKey, localKey) {
			return ErrEnrollmentRefused
		}
		return tx.Commit()
	}
	var exists int
	if e = tx.QueryRow(`SELECT count(*) FROM federation_peers WHERE instance_id=?`, t.Claims.Master).Scan(&exists); e != nil {
		return e
	}
	if exists != 0 {
		return ErrEnrollmentRefused
	}
	if _, e = tx.Exec(`INSERT INTO federation_peers(instance_id,pubkey,label,name,trusted_at,trust_level) VALUES(?,?,?,?,?,?)`, t.Claims.Master, t.Claims.MasterKey, "master-"+t.Claims.Master, "", dbTime(time.Now()), t.Claims.TrustLevel); e != nil {
		return e
	}
	if e = insertEnrollment(tx, "node", t, t.Claims.MasterKey, localKey); e != nil {
		return e
	}
	return tx.Commit()
}
