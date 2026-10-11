package hub

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const boardItemSchema = `
CREATE TABLE IF NOT EXISTS board_versions(board TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,item TEXT NOT NULL,version TEXT NOT NULL,blob TEXT NOT NULL,seq INTEGER NOT NULL,body TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(board,item,version),UNIQUE(board,blob),UNIQUE(board,item,seq),FOREIGN KEY(board,blob) REFERENCES board_blobs(board,id) ON DELETE CASCADE) STRICT;
CREATE TABLE IF NOT EXISTS board_pins(board TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,instance TEXT NOT NULL,item TEXT NOT NULL,version TEXT NOT NULL,PRIMARY KEY(board,instance,item),FOREIGN KEY(board,item,version) REFERENCES board_versions(board,item,version) ON DELETE CASCADE) STRICT;
`

func boardItemsCall(tx *sql.Tx, instance, method string, p boardParams, b Board) (any, error) {
	switch method {
	case "items.publish":
		if b.Frozen || (b.Role != "owner" && b.Role != "publisher") {
			return nil, adminErr(403, "board_publish", "board publication is not permitted")
		}
		v := p.Version
		if v == nil || v.Verify() != nil || v.Board != b.ID || v.Publisher != instance || v.Epoch != b.Epoch {
			return nil, adminErr(400, "board_version", "invalid signed version or key epoch")
		}
		var digest string
		var size int64
		if e := tx.QueryRow(`SELECT digest,bytes FROM board_blobs WHERE board=? AND id=? AND state='ready' AND (pinned=1 OR expires_at>?)`, b.ID, v.Blob, ts(time.Now())).Scan(&digest, &size); e != nil || digest != v.SHA256 || size != v.Bytes {
			return nil, adminErr(409, "board_blob", "verified ciphertext upload required")
		}
		var previous string
		var seq int64
		e := tx.QueryRow(`SELECT version,seq FROM board_versions WHERE board=? AND item=? ORDER BY seq DESC LIMIT 1`, b.ID, v.Item).Scan(&previous, &seq)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		if previous != v.Parent {
			return nil, adminErr(409, "board_changed", "item parent version changed")
		}
		raw, e := json.Marshal(v)
		if e != nil {
			return nil, e
		}
		used, e := boardStorageUsed(tx, b.ID)
		if e != nil {
			return nil, e
		}
		if int64(len(raw)) > b.QuotaBytes-used {
			return nil, adminErr(409, "board_quota", "board storage quota reached")
		}
		if _, e = tx.Exec(`INSERT INTO board_versions VALUES(?,?,?,?,?,?,?)`, b.ID, v.Item, v.Version, v.Blob, seq+1, string(raw), ts(time.Now())); e != nil {
			return nil, adminErr(409, "board_version", "version or blob already published")
		}
		return v, nil
	case "items.list", "items.versions":
		query := `SELECT v.body FROM board_versions v JOIN board_blobs o ON o.board=v.board AND o.id=v.blob WHERE v.board=? AND o.state='ready' AND (o.pinned=1 OR o.expires_at>?)`
		args := []any{b.ID, ts(time.Now())}
		if method == "items.list" {
			query += ` AND v.item>? AND v.seq=(SELECT max(latest.seq) FROM board_versions latest WHERE latest.board=v.board AND latest.item=v.item) ORDER BY v.item LIMIT 9`
			args = append(args, p.Cursor)
		} else {
			query += ` AND v.item=? AND v.version>? ORDER BY v.version LIMIT 9`
			args = append(args, p.Item, p.Cursor)
		}
		rows, e := tx.Query(query, args...)
		if e != nil {
			return nil, e
		}
		entries := []proto.BoardItemVersion{}
		for rows.Next() {
			var raw string
			if e = rows.Scan(&raw); e != nil {
				rows.Close()
				return nil, e
			}
			var v proto.BoardItemVersion
			if e = json.Unmarshal([]byte(raw), &v); e != nil {
				rows.Close()
				return nil, e
			}
			entries = append(entries, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		cursor := ""
		if len(entries) > 8 {
			entries = entries[:8]
			if method == "items.list" {
				cursor = entries[7].Item
			} else {
				cursor = entries[7].Version
			}
		}
		key := "items"
		if method == "items.versions" {
			key = "versions"
		}
		return map[string]any{key: entries, "next_cursor": cursor}, nil
	case "items.get":
		var raw string
		e := tx.QueryRow(`SELECT v.body FROM board_versions v JOIN board_blobs o ON o.board=v.board AND o.id=v.blob WHERE v.board=? AND v.item=? AND v.version=? AND o.state='ready' AND (o.pinned=1 OR o.expires_at>?)`, b.ID, p.Item, p.VersionID, ts(time.Now())).Scan(&raw)
		if errors.Is(e, sql.ErrNoRows) {
			return nil, adminErr(404, "board_version", "board version expired or unknown")
		}
		if e != nil {
			return nil, e
		}
		return json.RawMessage(raw), nil
	case "pins.set":
		var blob string
		if e := tx.QueryRow(`SELECT v.blob FROM board_versions v JOIN board_blobs o ON o.board=v.board AND o.id=v.blob WHERE v.board=? AND v.item=? AND v.version=? AND o.state='ready' AND (o.pinned=1 OR o.expires_at>?)`, b.ID, p.Item, p.VersionID, ts(time.Now())).Scan(&blob); e != nil {
			return nil, adminErr(404, "board_version", "unknown version to pin")
		}
		if _, e := tx.Exec(`INSERT INTO board_pins VALUES(?,?,?,?) ON CONFLICT(board,instance,item) DO UPDATE SET version=excluded.version`, b.ID, instance, p.Item, p.VersionID); e != nil {
			return nil, e
		}
		if _, e := tx.Exec(`UPDATE board_blobs SET pinned=EXISTS(SELECT 1 FROM board_versions v JOIN board_pins p ON p.board=v.board AND p.item=v.item AND p.version=v.version WHERE v.board=board_blobs.board AND v.blob=board_blobs.id) WHERE board=?`, b.ID); e != nil {
			return nil, e
		}
		return map[string]any{"ok": true, "pinned_version": p.VersionID}, nil
	case "pins.list":
		rows, e := tx.Query(`SELECT item,version FROM board_pins WHERE board=? AND instance=? AND item>? ORDER BY item LIMIT 101`, b.ID, instance, p.Cursor)
		if e != nil {
			return nil, e
		}
		type pin struct {
			Item    string `json:"item"`
			Version string `json:"version"`
		}
		pins := []pin{}
		for rows.Next() {
			var v pin
			if e = rows.Scan(&v.Item, &v.Version); e != nil {
				rows.Close()
				return nil, e
			}
			pins = append(pins, v)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		cursor := ""
		if len(pins) > 100 {
			pins = pins[:100]
			cursor = pins[99].Item
		}
		return map[string]any{"pins": pins, "next_cursor": cursor}, nil
	}
	return nil, adminErr(400, "operation", "unknown board item operation")
}
