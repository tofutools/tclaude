package hub

import (
	"crypto/ed25519"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/proto"
	"golang.org/x/sys/unix"
)

const adminSchema = `
CREATE TABLE IF NOT EXISTS hub_admins(instance_id TEXT PRIMARY KEY,pubkey BLOB NOT NULL,created_at TEXT NOT NULL,created_by TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS hub_admin_capabilities(instance_id TEXT NOT NULL REFERENCES hub_admins(instance_id) ON DELETE CASCADE ON UPDATE CASCADE,capability TEXT NOT NULL,PRIMARY KEY(instance_id,capability));
CREATE TABLE IF NOT EXISTS hub_admin_claim(singleton INTEGER PRIMARY KEY CHECK(singleton=1),token_hash TEXT NOT NULL,expires_at TEXT NOT NULL,consumed INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS hub_admin_replay(instance_id TEXT NOT NULL,request_id TEXT NOT NULL,expires_at TEXT NOT NULL,PRIMARY KEY(instance_id,request_id));
CREATE TABLE IF NOT EXISTS hub_admin_audit(sequence INTEGER PRIMARY KEY AUTOINCREMENT,at TEXT NOT NULL,instance TEXT NOT NULL,request_id TEXT NOT NULL,operation TEXT NOT NULL,status INTEGER NOT NULL,detail TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS hub_settings(key TEXT PRIMARY KEY,value INTEGER NOT NULL);
`

type AdminError struct {
	Status        int
	Code, Message string
}

func (e *AdminError) Error() string                   { return e.Message }
func adminErr(status int, code, message string) error { return &AdminError{status, code, message} }

type Admin struct {
	Instance     string    `json:"instance"`
	Name         string    `json:"name"`
	Fingerprint  string    `json:"fingerprint"`
	Capabilities []string  `json:"capabilities"`
	AddedAt      time.Time `json:"added_at"`
	AddedBy      string    `json:"added_by"`
}

func (s *Store) AdminGeneration() (string, error) {
	var value string
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO meta(k,v) VALUES('admin_generation',?)`, randHex(16)); err != nil {
		return "", err
	}
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k='admin_generation'`).Scan(&value)
	return value, err
}
func (s *Store) ClaimPath() string { return filepath.Join(filepath.Dir(s.path), "admin-claim.token") }
func readClaimFile(path string) (string, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Size() > 128 {
		return "", errors.New("claim file must be a regular 0600 file")
	}
	raw := make([]byte, st.Size())
	_, err = io.ReadFull(f, raw)
	return strings.TrimSpace(string(raw)), err
}
func writeClaimFile(path, token string) error {
	if st, err := os.Lstat(path); err == nil && (!st.Mode().IsRegular() || st.Mode().Perm() != 0600) {
		return errors.New("refusing non-regular or non-private claim file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".admin-claim-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.WriteString(token + "\n"); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

// PrepareAdminClaim is host-only bootstrap/reset. Restarting an unclaimed hub
// recovers lost/expired tokens; a claimed hub never silently resets its admins.
func (s *Store) PrepareAdminClaim(reset bool, now time.Time) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`UPDATE meta SET v=v WHERE k='admin_generation'`); err != nil {
		return "", err
	}
	var count int
	if err = tx.QueryRow(`SELECT count(*) FROM hub_admins`).Scan(&count); err != nil {
		return "", err
	}
	if count > 0 && !reset {
		return "", nil
	}
	if reset {
		if _, err = tx.Exec(`DELETE FROM hub_admins`); err != nil {
			return "", err
		}
		if _, err = tx.Exec(`DELETE FROM hub_admin_replay`); err != nil {
			return "", err
		}
	}
	var hash, expiry string
	var consumed bool
	e := tx.QueryRow(`SELECT token_hash,expires_at,consumed FROM hub_admin_claim WHERE singleton=1`).Scan(&hash, &expiry, &consumed)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return "", e
	}
	token, fileErr := readClaimFile(s.ClaimPath())
	if !reset && e == nil && !consumed && parseTS(expiry).After(now) && fileErr == nil && subtle.ConstantTimeCompare([]byte(hashToken(token)), []byte(hash)) == 1 {
		return token, tx.Commit()
	}
	if fileErr != nil && !errors.Is(fileErr, os.ErrNotExist) {
		return "", fileErr
	}
	token = "tchac_" + randHex(32)
	if err = writeClaimFile(s.ClaimPath(), token); err != nil {
		return "", err
	}
	if _, err = tx.Exec(`INSERT INTO hub_admin_claim(singleton,token_hash,expires_at,consumed) VALUES(1,?,?,0) ON CONFLICT(singleton) DO UPDATE SET token_hash=excluded.token_hash,expires_at=excluded.expires_at,consumed=0`, hashToken(token), ts(now.Add(24*time.Hour))); err != nil {
		return "", err
	}
	if _, err = tx.Exec(`INSERT INTO meta(k,v) VALUES('admin_generation',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, randHex(16)); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return token, nil
}

func (s *Store) ClaimAdmin(instance string, pub ed25519.PublicKey, token string, now time.Time) error {
	if len(token) != 70 || !strings.HasPrefix(token, "tchac_") {
		return adminErr(403, "claim_invalid", "invalid claim token")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`UPDATE hub_admin_claim SET consumed=consumed WHERE singleton=1`); err != nil {
		return err
	}
	var admins int
	if err = tx.QueryRow(`SELECT count(*) FROM hub_admins`).Scan(&admins); err != nil {
		return err
	}
	if admins != 0 {
		return adminErr(409, "claim_used", "hub already has an administrator")
	}
	var hash, expiry string
	var used bool
	if err = tx.QueryRow(`SELECT token_hash,expires_at,consumed FROM hub_admin_claim WHERE singleton=1`).Scan(&hash, &expiry, &used); err != nil {
		return adminErr(409, "claim_used", "no unclaimed token available")
	}
	if used {
		return adminErr(409, "claim_used", "claim token was already consumed")
	}
	if !parseTS(expiry).After(now) {
		return adminErr(409, "claim_expired", "claim token has expired; restart the unclaimed hub")
	}
	if subtle.ConstantTimeCompare([]byte(hashToken(token)), []byte(hash)) != 1 {
		return adminErr(403, "claim_invalid", "invalid claim token")
	}
	if proto.InstanceID(pub) != instance || len(pub) != ed25519.PublicKeySize {
		return adminErr(403, "bad_auth", "claim key mismatch")
	}
	if _, err = tx.Exec(`INSERT INTO hub_admins VALUES(?,?,?,?)`, instance, []byte(pub), ts(now), instance); err != nil {
		return err
	}
	for _, cap := range proto.HubAdminBootstrapCapabilities {
		if _, err = tx.Exec(`INSERT INTO hub_admin_capabilities VALUES(?,?)`, instance, cap); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`UPDATE hub_admin_claim SET consumed=1 WHERE singleton=1`); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	_ = os.Remove(s.ClaimPath())
	return nil
}

func (s *Store) Admins() ([]Admin, error) {
	rows, err := s.db.Query(`SELECT a.instance_id,coalesce(i.name,''),a.created_at,a.created_by FROM hub_admins a LEFT JOIN instances i ON i.instance_id=a.instance_id ORDER BY a.instance_id`)
	if err != nil {
		return nil, err
	}
	out := []Admin{}
	for rows.Next() {
		var a Admin
		var at string
		if err = rows.Scan(&a.Instance, &a.Name, &at, &a.AddedBy); err != nil {
			rows.Close()
			return nil, err
		}
		a.AddedAt = parseTS(at)
		a.Fingerprint = proto.InstanceFingerprint(a.Instance)
		out = append(out, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		caps, e := s.AdminCapabilities(out[i].Instance)
		if e != nil {
			return nil, e
		}
		out[i].Capabilities = caps
	}
	return out, nil
}
func (s *Store) AdminCapabilities(instance string) ([]string, error) {
	rows, err := s.db.Query(`SELECT capability FROM hub_admin_capabilities WHERE instance_id=? ORDER BY capability`, instance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	caps := []string{}
	for rows.Next() {
		var cap string
		if err = rows.Scan(&cap); err != nil {
			return nil, err
		}
		caps = append(caps, cap)
	}
	return caps, rows.Err()
}
func guardAdminLoss(tx *sql.Tx, instance string, keepManager bool) error {
	var owns, others int
	if err := tx.QueryRow(`SELECT count(*) FROM hub_admin_capabilities WHERE instance_id=? AND capability='hub.admins.manage'`, instance).Scan(&owns); err != nil {
		return err
	}
	if owns == 0 || keepManager {
		return nil
	}
	if err := tx.QueryRow(`SELECT count(*) FROM hub_admin_capabilities c JOIN instances i ON i.instance_id=c.instance_id WHERE c.instance_id<>? AND c.capability='hub.admins.manage' AND i.revoked=0 AND NOT EXISTS(SELECT 1 FROM identity_rotations r WHERE r.old_instance=i.instance_id AND r.state IN ('accepted','conflict','revoked','recovered'))`, instance).Scan(&others); err != nil {
		return err
	}
	if others == 0 {
		return adminErr(409, "last_admin", "cannot remove the last active admin manager")
	}
	return nil
}
func (s *Store) SetAdmin(instance, creator string, caps []string) error {
	if !proto.ValidInstanceID(instance) {
		return adminErr(400, "instance", "invalid instance ID")
	}
	caps = slices.Clone(caps)
	slices.Sort(caps)
	caps = slices.Compact(caps)
	if len(caps) == 0 {
		return adminErr(400, "capabilities", "select at least one capability")
	}
	for _, cap := range caps {
		if !slices.Contains(proto.HubAdminCapabilities, cap) {
			return adminErr(400, "capabilities", "unknown or unavailable hub capability")
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`UPDATE hub_admins SET created_at=created_at WHERE 0`); err != nil {
		return err
	}
	for _, cap := range caps {
		if proto.HubAdminElevatedCapabilities[cap] {
			var holds int
			if err = tx.QueryRow(`SELECT count(*) FROM hub_admin_capabilities c JOIN instances i ON i.instance_id=c.instance_id WHERE c.instance_id=? AND c.capability=? AND i.revoked=0 AND NOT EXISTS(SELECT 1 FROM identity_rotations r WHERE r.old_instance=i.instance_id AND r.state IN ('accepted','conflict','revoked','recovered'))`, creator, cap).Scan(&holds); err != nil {
				return err
			}
			if holds != 1 {
				return adminErr(403, "elevated_capability", "granting admin must already hold the elevated capability")
			}
		}
	}
	var pub []byte
	var revoked bool
	if err = tx.QueryRow(`SELECT pubkey,revoked FROM instances WHERE instance_id=?`, instance).Scan(&pub, &revoked); err != nil || revoked || len(pub) != ed25519.PublicKeySize {
		return adminErr(409, "instance", "admin must be an admitted instance with a verified key")
	}
	var retired int
	if err = tx.QueryRow(`SELECT count(*) FROM identity_rotations WHERE old_instance=? AND state IN ('accepted','conflict','revoked','recovered')`, instance).Scan(&retired); err != nil {
		return err
	}
	if retired != 0 {
		return adminErr(409, "instance", "retired or conflicted identities cannot administer the hub")
	}
	if err = guardAdminLoss(tx, instance, slices.Contains(caps, "hub.admins.manage")); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO hub_admins VALUES(?,?,?,?) ON CONFLICT(instance_id) DO UPDATE SET pubkey=excluded.pubkey`, instance, pub, ts(time.Now()), creator); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM hub_admin_capabilities WHERE instance_id=?`, instance); err != nil {
		return err
	}
	for _, cap := range caps {
		if _, err = tx.Exec(`INSERT INTO hub_admin_capabilities VALUES(?,?)`, instance, cap); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) RemoveAdmin(instance string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`UPDATE hub_admins SET created_at=created_at WHERE 0`); err != nil {
		return err
	}
	if err = guardAdminLoss(tx, instance, false); err != nil {
		return err
	}
	result, err := tx.Exec(`DELETE FROM hub_admins WHERE instance_id=?`, instance)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return adminErr(404, "admin", "no such admin")
	}
	return tx.Commit()
}

// AuthorizeAdminRequest persists the replay marker before any operation or
// response. A socket nonce/generation binds the signature; the DB protects
// replay across reconnects, processes and restarts and rereads live authority.
func (s *Store) AuthorizeAdminRequest(instance string, pub []byte, r *proto.HubAdminRequest, capability string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`DELETE FROM hub_admin_replay WHERE expires_at<?`, ts(time.Now().Add(-time.Minute))); err != nil {
		return err
	}
	var generation string
	if err = tx.QueryRow(`SELECT v FROM meta WHERE k='admin_generation'`).Scan(&generation); err != nil {
		return err
	}
	if r.Generation != generation {
		return adminErr(409, "admin_generation", "admin generation changed; retry explicitly")
	}
	var revoked bool
	if err = tx.QueryRow(`SELECT revoked FROM instances WHERE instance_id=?`, instance).Scan(&revoked); err != nil || revoked {
		return adminErr(403, "not_admitted", "instance is no longer admitted")
	}
	var retired int
	if err = tx.QueryRow(`SELECT count(*) FROM identity_rotations WHERE old_instance=? AND state IN ('accepted','conflict','revoked','recovered')`, instance).Scan(&retired); err != nil {
		return err
	}
	if retired != 0 {
		return adminErr(403, "not_admitted", "identity is retired or conflicted")
	}
	if capability != "" {
		var key []byte
		var count int
		if err = tx.QueryRow(`SELECT a.pubkey,count(c.capability) FROM hub_admins a LEFT JOIN hub_admin_capabilities c ON c.instance_id=a.instance_id AND c.capability=? WHERE a.instance_id=? GROUP BY a.instance_id`, capability, instance).Scan(&key, &count); err != nil || count != 1 || string(key) != string(pub) {
			return adminErr(403, "not_admin", "hub admin capability required")
		}
	}
	if _, err = tx.Exec(`INSERT INTO hub_admin_replay VALUES(?,?,?)`, instance, r.ID, ts(r.ExpiresAt)); err != nil {
		return adminErr(409, "replay", "hub admin request already used")
	}
	return tx.Commit()
}
func (s *Store) AuditAdmin(instance, id, operation string, status int, detail string) error {
	_, err := s.db.Exec(`INSERT INTO hub_admin_audit(at,instance,request_id,operation,status,detail) VALUES(?,?,?,?,?,?)`, ts(time.Now()), instance, id, operation, status, detail)
	if err == nil {
		_, err = s.db.Exec(`DELETE FROM hub_admin_audit WHERE sequence < (SELECT coalesce(max(sequence),0)-10000 FROM hub_admin_audit)`)
	}
	return err
}
func (s *Store) RevokeInvite(hash string) error {
	res, err := s.db.Exec(`DELETE FROM invites WHERE token_hash=?`, hash)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return adminErr(404, "invite", "no such invite")
	}
	return nil
}
func (s *Store) Claimable() (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT count(*) FROM hub_admins`).Scan(&count)
	return count == 0, err
}

func wrapAdminError(err error) *AdminError {
	var e *AdminError
	if errors.As(err, &e) {
		return e
	}
	return &AdminError{500, "hub", fmt.Sprintf("hub operation failed: %v", err)}
}
