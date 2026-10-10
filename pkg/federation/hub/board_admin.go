package hub

import (
	"encoding/json"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Hub moderation operates on metadata/ciphertext only. It cannot give an
// administrator a content key or install anything on a board member's node.
func (s *Store) moderateBoard(method string, raw json.RawMessage) (any, error) {
	var p struct {
		Board       string `json:"board"`
		Frozen      *bool  `json:"frozen"`
		QuotaBytes  *int64 `json:"quota_bytes"`
		MaxMembers  *int64 `json:"max_members"`
		MaxVersions *int64 `json:"max_versions"`
		Cursor      string `json:"cursor"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, adminErr(400, "invalid_arg", "invalid board moderation payload")
	}
	if method == "boards.list" {
		rows, err := s.db.Query(`SELECT id,name,epoch,frozen,quota_bytes,max_members,max_versions FROM boards WHERE id>? ORDER BY id LIMIT 101`, p.Cursor)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		items := []Board{}
		for rows.Next() {
			var b Board
			if err = rows.Scan(&b.ID, &b.Name, &b.Epoch, &b.Frozen, &b.QuotaBytes, &b.MaxMembers, &b.MaxVersions); err != nil {
				return nil, err
			}
			items = append(items, b)
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
		cursor := ""
		if len(items) > 100 {
			items = items[:100]
			cursor = items[99].ID
		}
		return map[string]any{"boards": items, "next_cursor": cursor}, nil
	}
	if !proto.ValidStreamID(p.Board) {
		return nil, adminErr(400, "board", "invalid board ID")
	}
	if method == "boards.delete" {
		r, err := s.db.Exec(`DELETE FROM boards WHERE id=?`, p.Board)
		if err != nil {
			return nil, err
		}
		n, _ := r.RowsAffected()
		if n == 0 {
			return nil, adminErr(404, "board_unknown", "unknown board")
		}
		return map[string]any{"ok": true}, nil
	}
	if method != "boards.patch" {
		return nil, adminErr(400, "operation", "unknown board moderation operation")
	}
	if p.QuotaBytes != nil && (*p.QuotaBytes < 1<<20 || *p.QuotaBytes > 16<<30) || p.MaxMembers != nil && (*p.MaxMembers < 1 || *p.MaxMembers > 100) || p.MaxVersions != nil && (*p.MaxVersions < 1 || *p.MaxVersions > 1000) {
		return nil, adminErr(400, "board_quota", "board limits exceed supported bounds")
	}
	// Reducing limits never silently ejects members or deletes pinned content.
	r, err := s.db.Exec(`UPDATE boards SET frozen=coalesce(?,frozen),quota_bytes=coalesce(?,quota_bytes),max_members=coalesce(?,max_members),max_versions=coalesce(?,max_versions) WHERE id=?`, p.Frozen, p.QuotaBytes, p.MaxMembers, p.MaxVersions, p.Board)
	if err != nil {
		return nil, err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return nil, adminErr(404, "board_unknown", "unknown board")
	}
	return map[string]any{"ok": true}, nil
}
