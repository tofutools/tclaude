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
CREATE TABLE IF NOT EXISTS board_members(board TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,instance TEXT NOT NULL,role TEXT NOT NULL CHECK(role IN ('owner','publisher','reader')),PRIMARY KEY(board,instance)) STRICT;
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
}
type boardParams struct {
	Board      string                     `json:"board"`
	Name       string                     `json:"name"`
	Instance   string                     `json:"instance"`
	Role       string                     `json:"role"`
	Token      string                     `json:"token"`
	TokenID    string                     `json:"token_id"`
	TTLSeconds int64                      `json:"ttl_seconds"`
	Epoch      int64                      `json:"epoch"`
	KeyPackage string                     `json:"key_package"`
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
func (s *Store) redeemBoardInvite(token, instance string, now time.Time) (string, error) {
	if !proto.ValidStreamID(token) || !proto.ValidInstanceID(instance) {
		return "", adminErr(403, "board_invite", "invalid board invitation")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var board, role, expiry, used string
	var epoch, current, max, count int64
	if err = tx.QueryRow(`SELECT board,role,expires_at,used_by,epoch FROM board_invites WHERE hash=?`, boardHash(token)).Scan(&board, &role, &expiry, &used, &epoch); err != nil || used != "" || !parseTS(expiry).After(now) {
		return "", adminErr(403, "board_invite", "invitation expired, used or unknown")
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
	if _, err = tx.Exec(`INSERT INTO board_members VALUES(?,?,?)`, board, instance, role); err != nil {
		return "", adminErr(409, "already_member", "already a member of this board")
	}
	if _, err = tx.Exec(`UPDATE board_invites SET used_by=? WHERE hash=?`, instance, boardHash(token)); err != nil {
		return "", err
	}
	return board, tx.Commit()
}

func (s *Store) boardCall(instance string, r *proto.BoardRequest, canCreate bool) (any, error) {
	var p boardParams
	if err := json.Unmarshal(r.Payload, &p); err != nil {
		return nil, adminErr(400, "invalid_arg", "invalid board payload")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Replay reservation is persisted in the same transaction as the mutation.
	if _, err = tx.Exec(`DELETE FROM board_replay WHERE expires_at<?`, ts(time.Now())); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`INSERT INTO board_replay VALUES(?,?,?)`, instance, r.ID, ts(r.ExpiresAt)); err != nil {
		return nil, adminErr(403, "replay", "board request already used")
	}
	var body any
	if r.Method == "boards.list" {
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
	} else if r.Method == "boards.create" {
		if !canCreate {
			return nil, adminErr(403, "board_create", "board creation requires fleet admission")
		}
		if len(p.Name) == 0 || len(p.Name) > 64 || proto.SafeName(p.Name, false) != p.Name || !proto.ValidStreamID(p.Board) {
			return nil, adminErr(400, "invalid_arg", "invalid board name or ID")
		}
		if len(p.Envelopes) != 1 || len(p.Envelopes[instance]) == 0 {
			return nil, adminErr(400, "board_key", "owner key envelope required")
		}
		if _, err = tx.Exec(`INSERT INTO boards(id,name,epoch,created_at) VALUES(?,?,1,?)`, p.Board, p.Name, ts(time.Now())); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(`INSERT INTO board_members VALUES(?,?,'owner')`, p.Board, instance); err != nil {
			return nil, err
		}
		if _, err = tx.Exec(`INSERT INTO board_keys VALUES(?,1,?,?)`, p.Board, instance, string(p.Envelopes[instance])); err != nil {
			return nil, err
		}
		body = map[string]any{"id": p.Board, "name": p.Name, "role": "owner", "epoch": 1}
	} else {
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
		case "boards.get":
			body = b
		case "members.list":
			rows, e := tx.Query(`SELECT instance,role FROM board_members WHERE board=? AND instance>? ORDER BY instance LIMIT 101`, p.Board, p.Cursor)
			if e != nil {
				return nil, e
			}
			items := []BoardMember{}
			for rows.Next() {
				var m BoardMember
				if e = rows.Scan(&m.Instance, &m.Role); e != nil {
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
			if !owner && !(r.Method == "members.remove" && p.Instance == instance) {
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
		case "invites.create":
			if !owner {
				return nil, adminErr(403, "board_owner", "board owner required")
			}
			if b.Frozen {
				return nil, adminErr(409, "board_frozen", "board frozen")
			}
			if (p.Role != "publisher" && p.Role != "reader") || p.TTLSeconds < 60 || p.TTLSeconds > 604800 || !proto.ValidStreamID(p.Token) || len(p.KeyPackage) == 0 || len(p.KeyPackage) > 8192 {
				return nil, adminErr(400, "invalid_arg", "invalid invitation role, TTL or sealed key package")
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
		case "keys.get":
			rows, e := tx.Query(`SELECT epoch,envelope FROM board_keys WHERE board=? AND instance=? ORDER BY epoch`, p.Board, instance)
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
			for _, id := range members {
				if len(p.Envelopes[id]) == 0 {
					return nil, adminErr(400, "board_key", "missing member key envelope")
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
	return body, nil
}
