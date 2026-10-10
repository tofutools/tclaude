package hub

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const boardSchema = `
CREATE TABLE IF NOT EXISTS boards(id TEXT PRIMARY KEY,name TEXT NOT NULL,epoch INTEGER NOT NULL CHECK(epoch>0),frozen INTEGER NOT NULL DEFAULT 0,quota_bytes INTEGER NOT NULL DEFAULT 268435456,max_members INTEGER NOT NULL DEFAULT 100,max_versions INTEGER NOT NULL DEFAULT 1000,created_at TEXT NOT NULL) STRICT;
CREATE TABLE IF NOT EXISTS board_members(board TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,instance TEXT NOT NULL,role TEXT NOT NULL CHECK(role IN ('owner','publisher','reader')),pub BLOB NOT NULL,key_proof BLOB NOT NULL,PRIMARY KEY(board,instance)) STRICT;
CREATE TABLE IF NOT EXISTS board_keys(board TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,epoch INTEGER NOT NULL,instance TEXT NOT NULL,envelope TEXT NOT NULL,PRIMARY KEY(board,epoch,instance)) STRICT;
CREATE TABLE IF NOT EXISTS board_invites(hash TEXT PRIMARY KEY,board TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,role TEXT NOT NULL CHECK(role IN ('publisher','reader')),epoch INTEGER NOT NULL,key_package TEXT NOT NULL,expires_at TEXT NOT NULL,used_by TEXT NOT NULL DEFAULT '') STRICT;
CREATE TABLE IF NOT EXISTS board_replay(instance TEXT NOT NULL,id TEXT NOT NULL,expires_at TEXT NOT NULL,PRIMARY KEY(instance,id)) STRICT;
`

type Board struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	Epoch       int64  `json:"epoch"`
	Frozen      bool   `json:"frozen"`
	QuotaBytes  int64  `json:"quota_bytes"`
	MaxMembers  int64  `json:"max_members"`
	MaxVersions int64  `json:"max_versions"`
}
type BoardMember struct {
	Instance string `json:"instance"`
	Role     string `json:"role"`
	Pub      []byte `json:"pubkey"`
	Proof    []byte `json:"key_proof"`
}
type boardParams struct {
	Blob       string                     `json:"blob"`
	Digest     string                     `json:"digest"`
	Bytes      int64                      `json:"bytes"`
	Key        []byte                     `json:"key"`
	Item       string                     `json:"item"`
	VersionID  string                     `json:"version_id"`
	Version    *proto.BoardItemVersion    `json:"version"`
	Board      string                     `json:"board"`
	Name       string                     `json:"name"`
	Instance   string                     `json:"instance"`
	Role       string                     `json:"role"`
	Token      string                     `json:"token"`
	TokenID    string                     `json:"token_id"`
	TTLSeconds int64                      `json:"ttl_seconds"`
	Epoch      int64                      `json:"epoch"`
	KeyPackage string                     `json:"key_package"`
	Proofs     map[string][]byte          `json:"key_proofs"`
	Envelopes  map[string]json.RawMessage `json:"envelopes"`
	Cursor     string                     `json:"cursor"`
	MaxEntries int                        `json:"max_entries"`
}

func boardHash(token string) string {
	v := sha256.Sum256([]byte(token))
	return hex.EncodeToString(v[:])
}
func (s *Store) hasBoardMembership(instance string) bool {
	var n int
	return s.db.QueryRow(`SELECT count(*) FROM board_members WHERE instance=?`, instance).Scan(&n) == nil && n > 0
}
func (s *Store) redeemBoardInvite(token, instance string, pub []byte, now time.Time) (string, error) {
	if !proto.ValidStreamID(token) || !proto.ValidInstanceID(instance) {
		return "", adminErr(403, "board_invite", "invalid board invitation")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var board, role, expiry, used string
	var epoch, current, max, count int64
	if err = tx.QueryRow(`SELECT board,role,expires_at,used_by,epoch FROM board_invites WHERE hash=?`, boardHash(token)).Scan(&board, &role, &expiry, &used, &epoch); err != nil || (used != "" && used != instance) || (used == "" && !parseTS(expiry).After(now)) {
		return "", adminErr(403, "board_invite", "invitation expired, used or unknown")
	}
	// Recover a consumed invitation only for the same key and an existing
	// membership. Never re-add a removed member or redeem for another identity.
	if used == instance {
		var n int
		if err = tx.QueryRow(`SELECT count(*) FROM board_members WHERE board=? AND instance=?`, board, instance).Scan(&n); err != nil {
			return "", err
		}
		if n != 1 {
			return "", adminErr(403, "board_invite", "board membership was removed")
		}
		return board, tx.Commit()
	}
	if err = tx.QueryRow(`SELECT epoch,max_members FROM boards WHERE id=? AND frozen=0`, board).Scan(&current, &max); err != nil || epoch != current {
		return "", adminErr(409, "board_changed", "board invitation key epoch changed or board frozen")
	}
	if err = tx.QueryRow(`SELECT count(*) FROM board_members WHERE board=?`, board).Scan(&count); err != nil {
		return "", err
	}
	if count >= max {
		return "", adminErr(409, "board_quota", "board member quota reached")
	}
	// Existing members cannot use a weaker invitation to replace their authority.
	if _, err = tx.Exec(`INSERT INTO board_members VALUES(?,?,?,?,?)`, board, instance, role, pub, []byte{}); err != nil {
		return "", adminErr(409, "already_member", "already a member of this board")
	}
	if _, err = tx.Exec(`UPDATE board_invites SET used_by=? WHERE hash=?`, instance, boardHash(token)); err != nil {
		return "", err
	}
	if _, err = tx.Exec(`INSERT INTO hub_admin_audit(at,instance,request_id,operation,status,detail) VALUES(?,?,?,'board.join',200,'')`, ts(now), instance, boardHash(token)); err != nil {
		return "", err
	}
	return board, tx.Commit()
}

func (s *Store) boardCall(instance string, pub []byte, r *proto.BoardRequest, canCreate bool) (any, error) {
	var p boardParams
	if err := json.Unmarshal(r.Payload, &p); err != nil {
		return nil, adminErr(400, "invalid_arg", "invalid board payload")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// Replay reservation is persisted in the same transaction as the mutation.
	if _, err = tx.Exec(`DELETE FROM board_replay WHERE expires_at<?`, ts(time.Now())); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`INSERT INTO board_replay VALUES(?,?,?)`, instance, r.ID, ts(r.ExpiresAt)); err != nil {
		return nil, adminErr(403, "replay", "board request already used")
	}
	var body any
	switch r.Method {
	case "boards.list":
		rows, e := tx.Query(`SELECT b.id,b.name,m.role,b.epoch,b.frozen,b.quota_bytes,b.max_members,b.max_versions FROM boards b JOIN board_members m ON m.board=b.id WHERE m.instance=? AND b.id>? ORDER BY b.id LIMIT 101`, instance, p.Cursor)
		if e != nil {
			return nil, e
		}
		items := []Board{}
		for rows.Next() {
			var b Board
			if e = rows.Scan(&b.ID, &b.Name, &b.Role, &b.Epoch, &b.Frozen, &b.QuotaBytes, &b.MaxMembers, &b.MaxVersions); e != nil {
				rows.Close()
				return nil, e
			}
			items = append(items, b)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		cursor := ""
		if len(items) > 100 {
			items = items[:100]
			cursor = items[99].ID
		}
		body = map[string]any{"boards": items, "next_cursor": cursor}
	case "boards.create":
		var boardCount int
		if err = tx.QueryRow(`SELECT count(*) FROM boards`).Scan(&boardCount); err != nil {
			return nil, err
		}
		if boardCount >= 100 {
			return nil, adminErr(409, "board_quota", "hub board quota reached")
		}
		if !canCreate {
			return nil, adminErr(403, "board_create", "board creation requires fleet admission")
		}
		if len(p.Name) == 0 || len(p.Name) > 64 || proto.SafeName(p.Name, false) != p.Name || !proto.ValidStreamID(p.Board) {
			return nil, adminErr(400, "invalid_arg", "invalid board name or ID")
		}
		if len(p.Envelopes) != 1 || len(p.Envelopes[instance]) == 0 || len(p.Proofs[instance]) != 32 {
			return nil, adminErr(400, "board_key", "owner key envelope required")
		}
		if _, err = tx.Exec(`INSERT INTO boards(id,name,epoch,created_at) VALUES(?,?,1,?)`, p.Board, p.Name, ts(time.Now())); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(`INSERT INTO board_members VALUES(?,?,'owner',?,?)`, p.Board, instance, pub, p.Proofs[instance]); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(`INSERT INTO board_keys VALUES(?,1,?,?)`, p.Board, instance, string(p.Envelopes[instance])); err != nil {
			return nil, err
		}
		body = map[string]any{"id": p.Board, "name": p.Name, "role": "owner", "epoch": 1}
	default:
		var b Board
		err = tx.QueryRow(`SELECT b.id,b.name,m.role,b.epoch,b.frozen,b.quota_bytes,b.max_members,b.max_versions FROM boards b JOIN board_members m ON m.board=b.id WHERE b.id=? AND m.instance=?`, p.Board, instance).Scan(&b.ID, &b.Name, &b.Role, &b.Epoch, &b.Frozen, &b.QuotaBytes, &b.MaxMembers, &b.MaxVersions)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, adminErr(404, "board_unknown", "board not available")
		}
		if err != nil {
			return nil, err
		}
		owner := b.Role == "owner"
		switch r.Method {
		case "blobs.put", "blobs.get":
			if !proto.ValidStreamID(p.Blob) || p.Bytes < 1 || p.Bytes > proto.MaxBoardItemBytes+64 || len(p.Key) != 32 {
				return nil, adminErr(400, "blob", "invalid blob stream descriptor")
			}
			if r.Method == "blobs.put" && (b.Frozen || (b.Role != "owner" && b.Role != "publisher")) {
				return nil, adminErr(403, "board_publish", "board publication refused")
			}
			body = map[string]any{"ok": true}
		case "items.publish", "items.list", "items.versions", "items.get", "pins.set", "pins.list":
			body, err = boardItemsCall(tx, instance, r.Method, p, b)
		case "boards.delete":
			if !owner {
				return nil, adminErr(403, "board_owner", "only an owner can delete a board")
			}
			_, err = tx.Exec(`DELETE FROM boards WHERE id=?`, p.Board)
			body = map[string]any{"ok": true}
		case "boards.get":
			body = b
		case "members.list":
			rows, e := tx.Query(`SELECT instance,role,pub,key_proof FROM board_members WHERE board=? AND instance>? ORDER BY instance LIMIT 101`, p.Board, p.Cursor)
			if e != nil {
				return nil, e
			}
			items := []BoardMember{}
			for rows.Next() {
				var m BoardMember
				if e = rows.Scan(&m.Instance, &m.Role, &m.Pub, &m.Proof); e != nil {
					rows.Close()
					return nil, e
				}
				items = append(items, m)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return nil, e
			}
			cursor := ""
			if len(items) > 100 {
				items = items[:100]
				cursor = items[99].Instance
			}
			body = map[string]any{"members": items, "next_cursor": cursor}
		case "members.set", "members.remove":
			if !owner && (r.Method != "members.remove" || p.Instance != instance) {
				return nil, adminErr(403, "board_owner", "board owner required")
			}
			if !proto.ValidInstanceID(p.Instance) {
				return nil, adminErr(400, "instance", "invalid instance")
			}
			var role string
			if e := tx.QueryRow(`SELECT role FROM board_members WHERE board=? AND instance=?`, p.Board, p.Instance).Scan(&role); e != nil {
				return nil, adminErr(404, "member_unknown", "unknown board member")
			}
			if role == "owner" && (r.Method == "members.remove" || p.Role != "owner") {
				var n int
				if err = tx.QueryRow(`SELECT count(*) FROM board_members WHERE board=? AND role='owner'`, p.Board).Scan(&n); err != nil {
					return nil, err
				}
				if n <= 1 {
					return nil, adminErr(409, "last_owner", "cannot remove the last board owner")
				}
			}
			if r.Method == "members.remove" {
				if _, err = tx.Exec(`DELETE FROM board_pins WHERE board=? AND instance=?`, p.Board, p.Instance); err != nil {
					return nil, err
				}
				if _, err = tx.Exec(`UPDATE board_blobs SET pinned=EXISTS(SELECT 1 FROM board_versions v JOIN board_pins p ON p.board=v.board AND p.item=v.item AND p.version=v.version WHERE v.board=board_blobs.board AND v.blob=board_blobs.id) WHERE board=?`, p.Board); err != nil {
					return nil, err
				}
				_, err = tx.Exec(`DELETE FROM board_members WHERE board=? AND instance=?`, p.Board, p.Instance)
				if err == nil {
					_, err = tx.Exec(`DELETE FROM board_keys WHERE board=? AND instance=?`, p.Board, p.Instance)
				}
			} else {
				if p.Role != "owner" && p.Role != "publisher" && p.Role != "reader" {
					return nil, adminErr(400, "role", "invalid board role")
				}
				_, err = tx.Exec(`UPDATE board_members SET role=? WHERE board=? AND instance=?`, p.Role, p.Board, p.Instance)
			}
			body = map[string]any{"ok": true}
		case "invites.list":
			if !owner {
				return nil, adminErr(403, "board_owner", "board owner required")
			}
			rows, e := tx.Query(`SELECT hash,role,epoch,expires_at,used_by FROM board_invites WHERE board=? AND hash>? ORDER BY hash LIMIT 51`, p.Board, p.Cursor)
			if e != nil {
				return nil, e
			}
			type invite struct {
				TokenID   string `json:"token_id"`
				Role      string `json:"role"`
				Epoch     int64  `json:"epoch"`
				ExpiresAt string `json:"expires_at"`
				UsedBy    string `json:"used_by"`
			}
			entries := []invite{}
			for rows.Next() {
				var v invite
				if e = rows.Scan(&v.TokenID, &v.Role, &v.Epoch, &v.ExpiresAt, &v.UsedBy); e != nil {
					rows.Close()
					return nil, e
				}
				entries = append(entries, v)
			}
			err = rows.Err()
			rows.Close()
			cursor := ""
			if len(entries) > 50 {
				entries = entries[:50]
				cursor = entries[49].TokenID
			}
			body = map[string]any{"invites": entries, "next_cursor": cursor}
		case "invites.create":
			if _, err = tx.Exec(`DELETE FROM board_invites WHERE board=? AND (expires_at<=? OR (used_by='' AND epoch<>?))`, p.Board, ts(time.Now()), b.Epoch); err != nil {
				return nil, err
			}
			var inviteCount int
			if err = tx.QueryRow(`SELECT count(*) FROM board_invites WHERE board=?`, p.Board).Scan(&inviteCount); err != nil {
				return nil, err
			}
			if inviteCount >= 100 {
				return nil, adminErr(409, "board_quota", "board invitation quota reached; revoke unused invitations")
			}
			if p.Epoch != b.Epoch {
				return nil, adminErr(409, "board_changed", "board key rotated; create a fresh invitation")
			}
			if !owner {
				return nil, adminErr(403, "board_owner", "board owner required")
			}
			if b.Frozen {
				return nil, adminErr(409, "board_frozen", "board frozen")
			}
			if (p.Role != "publisher" && p.Role != "reader") || p.TTLSeconds < 60 || p.TTLSeconds > 604800 || !proto.ValidStreamID(p.Token) || len(p.KeyPackage) == 0 || len(p.KeyPackage) > 8192 {
				return nil, adminErr(400, "invalid_arg", "invalid invitation role, TTL or sealed key package")
			}
			used, e := boardStorageUsed(tx, p.Board)
			if e != nil {
				return nil, e
			}
			if int64(len(p.KeyPackage)) > b.QuotaBytes-used {
				return nil, adminErr(409, "board_quota", "board ciphertext quota reached")
			}
			expiry := time.Now().Add(time.Duration(p.TTLSeconds) * time.Second)
			_, err = tx.Exec(`INSERT INTO board_invites VALUES(?,?,?,?,?,?, '')`, boardHash(p.Token), p.Board, p.Role, b.Epoch, p.KeyPackage, ts(expiry))
			body = map[string]any{"token_id": boardHash(p.Token), "expires_at": expiry}
		case "invites.revoke":
			if !owner {
				return nil, adminErr(403, "board_owner", "board owner required")
			}
			_, err = tx.Exec(`DELETE FROM board_invites WHERE board=? AND hash=?`, p.Board, p.TokenID)
			body = map[string]any{"ok": true}
		case "keys.join":
			var pack string
			var epoch int64
			if e := tx.QueryRow(`SELECT key_package,epoch FROM board_invites WHERE board=? AND hash=? AND used_by=?`, p.Board, boardHash(p.Token), instance).Scan(&pack, &epoch); e != nil {
				return nil, adminErr(404, "board_invite", "no redeemed key package for this instance")
			}
			body = map[string]any{"key_package": pack, "epoch": epoch}
		case "keys.install":
			if p.Epoch != b.Epoch || len(p.Envelopes) != 1 || len(p.Envelopes[instance]) == 0 || len(p.Proofs[instance]) != 32 {
				return nil, adminErr(409, "board_key", "install current key for this instance only")
			}
			used, e := boardStorageUsed(tx, p.Board)
			if e != nil {
				return nil, e
			}
			var replaced int64
			if e = tx.QueryRow(`SELECT coalesce(sum(length(CAST(envelope AS BLOB))),0) FROM board_keys WHERE board=? AND epoch=? AND instance=?`, p.Board, p.Epoch, instance).Scan(&replaced); e != nil {
				return nil, e
			}
			if int64(len(p.Envelopes[instance])) > b.QuotaBytes-used+replaced {
				return nil, adminErr(409, "board_quota", "board ciphertext quota reached")
			}
			_, err = tx.Exec(`INSERT INTO board_keys VALUES(?,?,?,?) ON CONFLICT(board,epoch,instance) DO UPDATE SET envelope=excluded.envelope`, p.Board, p.Epoch, instance, string(p.Envelopes[instance]))
			if err == nil {
				_, err = tx.Exec(`UPDATE board_members SET key_proof=? WHERE board=? AND instance=?`, p.Proofs[instance], p.Board, instance)
			}
			body = map[string]any{"ok": true}
		case "keys.get":
			if p.Epoch == 0 {
				p.Epoch = b.Epoch
			}
			if p.Epoch < 1 || p.Epoch > b.Epoch {
				return nil, adminErr(400, "board_key", "invalid key epoch")
			}
			rows, e := tx.Query(`SELECT epoch,envelope FROM board_keys WHERE board=? AND instance=? AND epoch=?`, p.Board, instance, p.Epoch)
			if e != nil {
				return nil, e
			}
			keys := map[int64]json.RawMessage{}
			for rows.Next() {
				var epoch int64
				var raw string
				if e = rows.Scan(&epoch, &raw); e != nil {
					rows.Close()
					return nil, e
				}
				keys[epoch] = json.RawMessage(raw)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return nil, e
			}
			body = map[string]any{"keys": keys}
		case "keys.rotate":
			if !owner {
				return nil, adminErr(403, "board_owner", "board owner required")
			}
			if p.Epoch != b.Epoch+1 {
				return nil, adminErr(409, "board_changed", "board key epoch changed")
			}
			rows, e := tx.Query(`SELECT instance FROM board_members WHERE board=?`, p.Board)
			if e != nil {
				return nil, e
			}
			members := []string{}
			for rows.Next() {
				var id string
				if e = rows.Scan(&id); e != nil {
					rows.Close()
					return nil, e
				}
				members = append(members, id)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return nil, e
			}
			if len(p.Envelopes) != len(members) {
				return nil, adminErr(400, "board_key", "every current member needs a new key envelope")
			}
			used, e := boardStorageUsed(tx, p.Board)
			if e != nil {
				return nil, e
			}
			var additional int64
			for _, box := range p.Envelopes {
				additional += int64(len(box))
			}
			if additional > b.QuotaBytes-used {
				return nil, adminErr(409, "board_quota", "board ciphertext quota reached")
			}
			for _, id := range members {
				if len(p.Envelopes[id]) == 0 || len(p.Proofs[id]) != 32 {
					return nil, adminErr(400, "board_key", "missing member key envelope")
				}
				if _, err = tx.Exec(`UPDATE board_members SET key_proof=? WHERE board=? AND instance=?`, p.Proofs[id], p.Board, id); err != nil {
					return nil, err
				}
				if _, err = tx.Exec(`INSERT INTO board_keys VALUES(?,?,?,?)`, p.Board, p.Epoch, id, string(p.Envelopes[id])); err != nil {
					return nil, err
				}
			}
			_, err = tx.Exec(`UPDATE boards SET epoch=? WHERE id=?`, p.Epoch, p.Board)
			body = map[string]any{"epoch": p.Epoch}
		default:
			return nil, adminErr(400, "operation", "unknown board operation")
		}
	}
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if r.Method == "boards.delete" {
		if err = s.removeDeletedBoardFiles(p.Board); err != nil {
			return nil, adminErr(503, "board_cleanup", "board deleted but ciphertext cleanup failed: "+err.Error())
		}
	}
	return body, nil
}

// Keys and invitation packages also consume the board's ciphertext budget;
// otherwise repeated rotations could bypass the blob quota indefinitely.
func boardStorageUsed(tx *sql.Tx, board string) (int64, error) {
	var used int64
	err := tx.QueryRow(`SELECT (SELECT coalesce(sum(bytes),0) FROM board_blobs WHERE board=?)+(SELECT coalesce(sum(length(CAST(envelope AS BLOB))),0) FROM board_keys WHERE board=?)+(SELECT coalesce(sum(length(CAST(key_package AS BLOB))),0) FROM board_invites WHERE board=?)+(SELECT coalesce(sum(length(CAST(body AS BLOB))),0) FROM board_versions WHERE board=?)`, board, board, board, board).Scan(&used)
	return used, err
}
