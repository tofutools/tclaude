package hub

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// DefaultSpace is the space instances join when no other is named.
const DefaultSpace = "default"

// Store is the hub's private SQLite state: admitted instances, the spaces
// that scope their visibility, and single-use invites. The hub never stores
// messages.
type Store struct {
	db   *sql.DB
	path string
}

// Instance is one admitted (or revoked) instance.
type Instance struct {
	ID         string
	PubKey     []byte
	Name       string
	Version    string
	AdmittedAt time.Time
	LastSeen   time.Time
	Revoked    bool
	Spaces     []string
}

// Invite is a single-use admission token, stored only as a hash.
type Invite struct {
	Hash      string
	Space     string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedBy    string
	UsedAt    time.Time
}

// OpenStore opens (creating) the hub database at path.
func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + adminSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("hub schema: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS instances (
	instance_id TEXT PRIMARY KEY,
	pubkey      BLOB,
	name        TEXT NOT NULL DEFAULT '',
	version     TEXT NOT NULL DEFAULT '',
	admitted_at TEXT NOT NULL,
	last_seen   TEXT NOT NULL DEFAULT '',
	revoked     INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS instance_spaces (
	instance_id TEXT NOT NULL REFERENCES instances(instance_id) ON DELETE CASCADE,
	space       TEXT NOT NULL,
	PRIMARY KEY (instance_id, space)
);
CREATE TABLE IF NOT EXISTS identity_rotations (
 old_instance TEXT PRIMARY KEY,new_instance TEXT NOT NULL,statement TEXT NOT NULL,
 state TEXT NOT NULL,received_at TEXT NOT NULL,accept_after TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS invites (
	token_hash TEXT PRIMARY KEY,
	space      TEXT NOT NULL,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	used_by    TEXT NOT NULL DEFAULT '',
	used_at    TEXT NOT NULL DEFAULT ''
);
`

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// HubID returns the hub's stable random id, creating it on first use.
func (s *Store) HubID() (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k='hub_id'`).Scan(&v)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	v = "hub_" + randHex(10)
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO meta(k,v) VALUES('hub_id',?)`, v); err != nil {
		return "", err
	}
	return s.HubID()
}

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// Admit admits instanceID into the given spaces (DefaultSpace when none),
// clearing any revocation. Admission is by id: the key arrives at hello and
// must derive to the id, so no separate key pinning is needed.
func (s *Store) Admit(instanceID string, spaces ...string) error {
	if !proto.ValidInstanceID(instanceID) {
		return fmt.Errorf("invalid instance id %q", instanceID)
	}
	if len(spaces) == 0 {
		spaces = []string{DefaultSpace}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO instances(instance_id, admitted_at) VALUES(?,?)
		ON CONFLICT(instance_id) DO UPDATE SET revoked=0`, instanceID, ts(time.Now())); err != nil {
		return err
	}
	for _, sp := range spaces {
		if sp == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO instance_spaces(instance_id, space) VALUES(?,?)`, instanceID, sp); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Revoke marks instanceID revoked; a live connection is dropped by the hub's
// policy refresh.
func (s *Store) Revoke(instanceID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE instances SET revoked=revoked WHERE instance_id=?`, instanceID); err != nil {
		return err
	}
	if err = guardAdminLoss(tx, instanceID, false); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE instances SET revoked=1 WHERE instance_id=?`, instanceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown instance %q", instanceID)
	}
	if _, err = tx.Exec(`DELETE FROM hub_admins WHERE instance_id=?`, instanceID); err != nil {
		return err
	}
	return tx.Commit()
}

// SetSpaces replaces instanceID's spaces.
func (s *Store) SetSpaces(instanceID string, spaces []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM instance_spaces WHERE instance_id=?`, instanceID); err != nil {
		return err
	}
	for _, sp := range spaces {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO instance_spaces(instance_id, space) VALUES(?,?)`, instanceID, sp); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RecordSeen stores the presented key, name, and version and bumps
// last_seen.
func (s *Store) RecordSeen(instanceID string, pub []byte, name, version string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE instances SET pubkey=?, name=?, version=?, last_seen=? WHERE instance_id=?`,
		pub, name, version, ts(at), instanceID)
	return err
}

// Get returns one instance, or nil when unknown.
func (s *Store) Get(instanceID string) (*Instance, error) {
	all, err := s.list(`WHERE instance_id=?`, instanceID)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return &all[0], nil
}

// List returns every known instance.
func (s *Store) List() ([]Instance, error) { return s.list("") }

func (s *Store) list(where string, args ...any) ([]Instance, error) {
	rows, err := s.db.Query(`SELECT instance_id, pubkey, name, version, admitted_at, last_seen, revoked FROM instances `+where+` ORDER BY instance_id`, args...)
	if err != nil {
		return nil, err
	}
	var out []Instance
	for rows.Next() {
		var in Instance
		var adm, seen string
		var rev int
		if err := rows.Scan(&in.ID, &in.PubKey, &in.Name, &in.Version, &adm, &seen, &rev); err != nil {
			_ = rows.Close()
			return nil, err
		}
		in.AdmittedAt, in.LastSeen, in.Revoked = parseTS(adm), parseTS(seen), rev != 0
		out = append(out, in)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	spaces, err := s.allSpaces()
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Spaces = spaces[out[i].ID]
	}
	return out, nil
}

func (s *Store) allSpaces() (map[string][]string, error) {
	rows, err := s.db.Query(`SELECT instance_id, space FROM instance_spaces ORDER BY space`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	m := map[string][]string{}
	for rows.Next() {
		var id, sp string
		if err := rows.Scan(&id, &sp); err != nil {
			return nil, err
		}
		m[id] = append(m[id], sp)
	}
	return m, rows.Err()
}

// CreateInvite mints a single-use invite token for space, valid for ttl.
// Only its hash is stored; the token is returned once.
func (s *Store) CreateInvite(space string, ttl time.Duration) (string, error) {
	if space == "" {
		space = DefaultSpace
	}
	tok := "tchi_" + randHex(24)
	now := time.Now()
	_, err := s.db.Exec(`INSERT INTO invites(token_hash, space, created_at, expires_at) VALUES(?,?,?,?)`,
		hashToken(tok), space, ts(now), ts(now.Add(ttl)))
	return tok, err
}

// ErrInvalidInvite is returned for unknown, used, or expired invites.
var ErrInvalidInvite = errors.New("invalid, used, or expired invite")

// ErrRevoked is returned when a revoked instance presents an invite.
var ErrRevoked = errors.New("instance is revoked; ask the hub admin to admit it again")

// RedeemInvite consumes token and admits instanceID into its space.
func (s *Store) RedeemInvite(token, instanceID string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var space, exp, used string
	err = tx.QueryRow(`SELECT space, expires_at, used_by FROM invites WHERE token_hash=?`, hashToken(token)).Scan(&space, &exp, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidInvite
	}
	if err != nil {
		return err
	}
	if used != "" || now.After(parseTS(exp)) {
		return ErrInvalidInvite
	}
	// Revocation is sticky: an invite cannot undo it, only an explicit
	// admin `admit` can.
	var revoked int
	switch err := tx.QueryRow(`SELECT revoked FROM instances WHERE instance_id=?`, instanceID).Scan(&revoked); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case revoked != 0:
		return ErrRevoked
	}
	if _, err := tx.Exec(`UPDATE invites SET used_by=?, used_at=? WHERE token_hash=?`, instanceID, ts(now), hashToken(token)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO instances(instance_id, admitted_at) VALUES(?,?)`, instanceID, ts(now)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO instance_spaces(instance_id, space) VALUES(?,?)`, instanceID, space); err != nil {
		return err
	}
	return tx.Commit()
}

// ListInvites returns every invite (hashes only).
func (s *Store) ListInvites() ([]Invite, error) {
	rows, err := s.db.Query(`SELECT token_hash, space, created_at, expires_at, used_by, used_at FROM invites ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Invite
	for rows.Next() {
		var in Invite
		var c, e, u string
		if err := rows.Scan(&in.Hash, &in.Space, &c, &e, &in.UsedBy, &u); err != nil {
			return nil, err
		}
		in.CreatedAt, in.ExpiresAt, in.UsedAt = parseTS(c), parseTS(e), parseTS(u)
		out = append(out, in)
	}
	return out, rows.Err()
}

// admittedSnapshot is the policy view the hub caches between refreshes.
type admittedSnapshot struct {
	admitted map[string]bool
	spaces   map[string][]string
}

func (s *Store) snapshot() (*admittedSnapshot, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	snap := &admittedSnapshot{admitted: map[string]bool{}, spaces: map[string][]string{}}
	for _, in := range all {
		if !in.Revoked {
			snap.admitted[in.ID] = true
			sp := append([]string(nil), in.Spaces...)
			sort.Strings(sp)
			snap.spaces[in.ID] = sp
		}
	}
	return snap, nil
}

// visible reports whether a and b share a space.
func (p *admittedSnapshot) visible(a, b string) bool {
	if a == b || !p.admitted[a] || !p.admitted[b] {
		return false
	}
	for _, x := range p.spaces[a] {
		for _, y := range p.spaces[b] {
			if x == y {
				return true
			}
		}
	}
	return false
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
