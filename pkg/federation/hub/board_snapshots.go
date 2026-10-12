package hub

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Additive tables deliberately leave the existing receipt/schema untouched.
const boardSnapshotSchema = `
CREATE TABLE IF NOT EXISTS board_snapshot_blobs(board TEXT NOT NULL,blob TEXT NOT NULL,publisher TEXT NOT NULL,expires_at TEXT NOT NULL,PRIMARY KEY(board,blob),FOREIGN KEY(board,blob) REFERENCES board_blobs(board,id) ON DELETE CASCADE) STRICT;
CREATE TABLE IF NOT EXISTS board_snapshot_versions(board TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,item TEXT NOT NULL,version TEXT NOT NULL,seq INTEGER NOT NULL,body TEXT NOT NULL,publisher TEXT NOT NULL,expires_at TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(board,item,version),UNIQUE(board,item,seq)) STRICT;
CREATE TABLE IF NOT EXISTS board_snapshot_chunks(board TEXT NOT NULL,item TEXT NOT NULL,version TEXT NOT NULL,blob TEXT NOT NULL,PRIMARY KEY(board,blob),FOREIGN KEY(board,item,version) REFERENCES board_snapshot_versions(board,item,version) ON DELETE CASCADE,FOREIGN KEY(board,blob) REFERENCES board_snapshot_blobs(board,blob)) STRICT;
CREATE TABLE IF NOT EXISTS board_snapshot_pins(board TEXT NOT NULL,instance TEXT NOT NULL,item TEXT NOT NULL,version TEXT NOT NULL,PRIMARY KEY(board,instance,item),FOREIGN KEY(board,item,version) REFERENCES board_snapshot_versions(board,item,version) ON DELETE CASCADE) STRICT;
CREATE TABLE IF NOT EXISTS board_snapshot_deleted(board TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,item TEXT NOT NULL,version TEXT NOT NULL,publisher TEXT NOT NULL,PRIMARY KEY(board,item,version)) STRICT;
CREATE TABLE IF NOT EXISTS board_snapshot_cleanup(board TEXT NOT NULL,item TEXT NOT NULL,version TEXT NOT NULL,blob TEXT NOT NULL,PRIMARY KEY(board,blob),FOREIGN KEY(board,item,version) REFERENCES board_snapshot_deleted(board,item,version) ON DELETE CASCADE) STRICT;
`

func boardSnapshotCapabilities() any {
	return map[string]any{"capabilities": []string{proto.BoardSnapshotsCapability}, "max_payload_bytes": proto.MaxBoardSnapshotBytes, "max_chunks": proto.MaxBoardSnapshotChunks, "default_ttl_seconds": int64(proto.DefaultBoardSnapshotTTL / time.Second), "max_ttl_seconds": int64(proto.MaxBoardSnapshotTTL / time.Second)}
}
func snapshotTS(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

func snapshotExpiryValid(expiry, now time.Time) bool {
	return !expiry.IsZero() && expiry.After(now) && !expiry.After(now.Add(proto.MaxBoardSnapshotTTL))
}

func (s *Store) boardSnapshotsCall(tx *sql.Tx, instance, method string, p boardParams, b Board) (any, error) {
	now := s.now()
	switch method {
	case "snapshots.publish":
		if b.Frozen || (b.Role != "owner" && b.Role != "publisher") {
			return nil, adminErr(403, "board_publish", "board publication is not permitted")
		}
		v := p.Snapshot
		if v == nil || v.Verify() != nil || v.Board != b.ID || v.Publisher != instance || v.Epoch != b.Epoch || !snapshotExpiryValid(v.ExpiresAt, now) {
			return nil, adminErr(400, "board_snapshot", "invalid snapshot receipt, expiry or epoch")
		}
		var exists int
		if e := tx.QueryRow(`SELECT (SELECT count(*) FROM board_versions WHERE board=? AND item=?) + (SELECT count(*) FROM board_snapshot_deleted WHERE board=? AND item=? AND version=?)`, b.ID, v.Item, b.ID, v.Item, v.Version).Scan(&exists); e != nil {
			return nil, e
		}
		if exists > 0 {
			return nil, adminErr(409, "board_version", "item or version unavailable")
		}
		for _, id := range v.Recipients {
			if e := tx.QueryRow(`SELECT count(*) FROM board_members WHERE board=? AND instance=?`, b.ID, id).Scan(&exists); e != nil {
				return nil, e
			}
			if exists != 1 {
				return nil, adminErr(400, "board_recipient", "snapshot recipient is not a current board member")
			}
		}
		for _, chunk := range v.Chunks {
			var digest, publisher, expiry string
			var size int64
			e := tx.QueryRow(`SELECT x.digest,x.bytes,c.publisher,c.expires_at FROM board_blobs x JOIN board_snapshot_blobs c ON c.board=x.board AND c.blob=x.id WHERE x.board=? AND x.id=? AND x.state='ready' AND c.expires_at>? AND NOT EXISTS(SELECT 1 FROM board_snapshot_cleanup d WHERE d.board=x.board AND d.blob=x.id)`, b.ID, chunk.Blob, snapshotTS(now)).Scan(&digest, &size, &publisher, &expiry)
			if e != nil || digest != chunk.SHA256 || size != chunk.Bytes || publisher != instance || !parseTS(expiry).Equal(v.ExpiresAt) {
				return nil, adminErr(409, "board_blob", "matching ready snapshot chunk required")
			}
		}
		var previous string
		var seq int64
		e := tx.QueryRow(`SELECT version,seq FROM board_snapshot_versions WHERE board=? AND item=? ORDER BY seq DESC LIMIT 1`, b.ID, v.Item).Scan(&previous, &seq)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		if previous != v.Parent {
			return nil, adminErr(409, "board_changed", "snapshot parent version changed")
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
		if _, e = tx.Exec(`INSERT INTO board_snapshot_versions VALUES(?,?,?,?,?,?,?,?)`, b.ID, v.Item, v.Version, seq+1, string(raw), instance, snapshotTS(v.ExpiresAt), snapshotTS(now)); e != nil {
			return nil, adminErr(409, "board_version", "snapshot version already published")
		}
		for _, chunk := range v.Chunks {
			if _, e = tx.Exec(`INSERT INTO board_snapshot_chunks VALUES(?,?,?,?)`, b.ID, v.Item, v.Version, chunk.Blob); e != nil {
				return nil, adminErr(409, "board_blob", "snapshot chunk already published")
			}
		}
		return v, nil
	case "snapshots.get":
		var raw string
		e := tx.QueryRow(`SELECT body FROM board_snapshot_versions WHERE board=? AND item=? AND version=? AND expires_at>?`, b.ID, p.Item, p.VersionID, snapshotTS(now)).Scan(&raw)
		if errors.Is(e, sql.ErrNoRows) {
			return nil, adminErr(404, "board_version", "snapshot expired or unknown")
		}
		if e != nil {
			return nil, e
		}
		return json.RawMessage(raw), nil
	case "pins.set":
		var expiry string
		if e := tx.QueryRow(`SELECT expires_at FROM board_snapshot_versions WHERE board=? AND item=? AND version=? AND expires_at>?`, b.ID, p.Item, p.VersionID, snapshotTS(now)).Scan(&expiry); e != nil {
			return nil, adminErr(404, "board_version", "snapshot expired or unknown")
		}
		if _, e := tx.Exec(`INSERT INTO board_snapshot_pins VALUES(?,?,?,?) ON CONFLICT(board,instance,item) DO UPDATE SET version=excluded.version`, b.ID, instance, p.Item, p.VersionID); e != nil {
			return nil, e
		}
		return map[string]any{"ok": true, "pinned_version": p.VersionID, "expires_at": expiry}, nil
	case "items.delete":
		var publisher string
		e := tx.QueryRow(`SELECT publisher FROM board_snapshot_versions WHERE board=? AND item=? AND version=? UNION ALL SELECT publisher FROM board_snapshot_deleted WHERE board=? AND item=? AND version=? LIMIT 1`, b.ID, p.Item, p.VersionID, b.ID, p.Item, p.VersionID).Scan(&publisher)
		if errors.Is(e, sql.ErrNoRows) {
			return nil, adminErr(404, "board_version", "snapshot unknown")
		}
		if e != nil {
			return nil, e
		}
		if b.Role != "owner" && instance != publisher {
			return nil, adminErr(403, "board_publish", "snapshot publisher or board owner required")
		}
		if e = markSnapshotDeleted(tx, b.ID, p.Item, p.VersionID, publisher); e != nil {
			return nil, e
		}
		return map[string]any{"ok": true}, nil
	}
	return nil, adminErr(400, "operation", "unknown snapshot operation")
}

func markSnapshotDeleted(tx *sql.Tx, board, item, version, publisher string) error {
	if _, e := tx.Exec(`INSERT OR IGNORE INTO board_snapshot_deleted VALUES(?,?,?,?)`, board, item, version, publisher); e != nil {
		return e
	}
	if _, e := tx.Exec(`INSERT OR IGNORE INTO board_snapshot_cleanup SELECT board,item,version,blob FROM board_snapshot_chunks WHERE board=? AND item=? AND version=?`, board, item, version); e != nil {
		return e
	}
	_, e := tx.Exec(`DELETE FROM board_snapshot_versions WHERE board=? AND item=? AND version=?`, board, item, version)
	return e
}

// Revocation commits before filesystem cleanup. Failed cleanup keeps its ledger
// and quota reservation, so a repeated delete or a later sweep can finish it.
func (s *Store) cleanupSnapshotDeletions(board, item, version string) error {
	query := `SELECT board,blob FROM board_snapshot_cleanup`
	args := []any{}
	if board != "" {
		query += ` WHERE board=? AND item=? AND version=?`
		args = []any{board, item, version}
	}
	rows, e := s.db.Query(query, args...)
	if e != nil {
		return e
	}
	type entry struct{ board, blob string }
	all := []entry{}
	for rows.Next() {
		var v entry
		if e = rows.Scan(&v.board, &v.blob); e != nil {
			rows.Close()
			return e
		}
		all = append(all, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, v := range all {
		root, e := s.boardBlobRoot(v.board)
		if e != nil {
			return e
		}
		e = root.Remove(v.blob)
		_ = root.Close()
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		tx, e := s.db.Begin()
		if e != nil {
			return e
		}
		if _, e = tx.Exec(`DELETE FROM board_blobs WHERE board=? AND id=?`, v.board, v.blob); e == nil {
			_, e = tx.Exec(`DELETE FROM board_snapshot_cleanup WHERE board=? AND blob=?`, v.board, v.blob)
		}
		if e != nil {
			_ = tx.Rollback()
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	return nil
}
func (s *Store) expireSnapshots() error {
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	rows, e := tx.Query(`SELECT board,item,version,publisher FROM board_snapshot_versions WHERE expires_at<=?`, snapshotTS(s.now()))
	if e != nil {
		return e
	}
	type entry struct{ board, item, version, publisher string }
	all := []entry{}
	for rows.Next() {
		var v entry
		if e = rows.Scan(&v.board, &v.item, &v.version, &v.publisher); e != nil {
			rows.Close()
			return e
		}
		all = append(all, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, v := range all {
		if e = markSnapshotDeleted(tx, v.board, v.item, v.version, v.publisher); e != nil {
			return e
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	return s.cleanupSnapshotDeletions("", "", "")
}

// Opt-in mixed pages preserve existing responses for older readers. The combined
// page is bounded before splitting receipts into their two distinct wire types.
func (s *Store) boardSnapshotCombinedCall(tx *sql.Tx, instance, method string, p boardParams, b Board) (any, error) {
	now := snapshotTS(s.now())
	if method == "pins.list" {
		rows, e := tx.Query(`SELECT item,version FROM board_pins WHERE board=? AND instance=? AND item>? UNION ALL SELECT p.item,p.version FROM board_snapshot_pins p JOIN board_snapshot_versions v ON v.board=p.board AND v.item=p.item AND v.version=p.version WHERE p.board=? AND p.instance=? AND p.item>? AND v.expires_at>? ORDER BY item LIMIT 101`, b.ID, instance, p.Cursor, b.ID, instance, p.Cursor, now)
		if e != nil {
			return nil, e
		}
		pins := []map[string]string{}
		for rows.Next() {
			var item, ver string
			if e = rows.Scan(&item, &ver); e != nil {
				rows.Close()
				return nil, e
			}
			pins = append(pins, map[string]string{"item": item, "version": ver})
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		cursor := ""
		if len(pins) > 100 {
			pins = pins[:100]
			cursor = pins[99]["item"]
		}
		return map[string]any{"pins": pins, "next_cursor": cursor}, nil
	}
	legacy := `SELECT v.item,v.version,v.seq,v.body,v.created_at,0 AS snapshot FROM board_versions v JOIN board_blobs o ON o.board=v.board AND o.id=v.blob WHERE v.board=? AND o.state='ready' AND (o.pinned=1 OR o.expires_at>?)`
	snapshot := `SELECT v.item,v.version,v.seq,v.body,v.created_at,1 AS snapshot FROM board_snapshot_versions v WHERE v.board=? AND v.expires_at>?`
	args := []any{b.ID, ts(s.now())}
	if method == "items.list" {
		legacy += ` AND v.seq=(SELECT max(seq) FROM board_versions x WHERE x.board=v.board AND x.item=v.item)`
		snapshot += ` AND v.seq=(SELECT max(seq) FROM board_snapshot_versions x WHERE x.board=v.board AND x.item=v.item)`
	}
	args = append(args, b.ID, now)
	query := `SELECT item,version,seq,body,created_at,snapshot FROM (` + legacy + ` UNION ALL ` + snapshot + `) WHERE `
	if method == "items.list" {
		query += `item>? ORDER BY item LIMIT 9`
		args = append(args, p.Cursor)
	} else {
		query += `item=? AND version>? ORDER BY version LIMIT 9`
		args = append(args, p.Item, p.Cursor)
	}
	rows, e := tx.Query(query, args...)
	if e != nil {
		return nil, e
	}
	type entry struct {
		item, ver, body, created string
		seq                      int64
		snapshot                 int
	}
	all := []entry{}
	for rows.Next() {
		var v entry
		if e = rows.Scan(&v.item, &v.ver, &v.seq, &v.body, &v.created, &v.snapshot); e != nil {
			rows.Close()
			return nil, e
		}
		all = append(all, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	cursor := ""
	if len(all) > 8 {
		all = all[:8]
		if method == "items.list" {
			cursor = all[7].item
		} else {
			cursor = all[7].ver
		}
	}
	items := []json.RawMessage{}
	snapshots := []json.RawMessage{}
	catalog := map[string]proto.BoardCatalogMetadata{}
	for _, v := range all {
		if v.snapshot == 1 {
			snapshots = append(snapshots, json.RawMessage(v.body))
		} else {
			items = append(items, json.RawMessage(v.body))
		}
		catalog[v.item+"/"+v.ver] = proto.BoardCatalogMetadata{UpdatedAt: v.created, Seq: v.seq}
	}
	key := "items"
	if method == "items.versions" {
		key = "versions"
	}
	return map[string]any{key: items, "snapshots": snapshots, "next_cursor": cursor, "catalog_metadata": catalog}, nil
}
