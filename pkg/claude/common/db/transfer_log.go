package db

import (
	"database/sql"
	"fmt"
	"time"
)

// Transfer kinds stored in agent_transfer_log.kind.
const (
	TransferKindExport = "export"
	TransferKindImport = "import"
)

// TransferLogEntry is one row of agent_transfer_log — the persistent
// audit trail for per-group export / import (see migrateV39toV40).
//
// For an export the import-specific fields (SourceHome, SourceOS,
// TargetDir, ConvRemaps) are empty; SourceGroup and ResultGroup both
// name the exported group. For an import they describe the move:
// SourceGroup is the group name inside the export file, ResultGroup is
// the name it landed under locally (possibly chosen via --as), and
// ConvRemaps is a JSON object mapping each collided source conv-id to
// the fresh id minted for it.
type TransferLogEntry struct {
	ID            int64
	Kind          string
	At            time.Time
	FormatVersion int
	SourceGroup   string
	SourceHome    string
	SourceOS      string
	ResultGroup   string
	TargetDir     string
	ConvRemaps    string
	AgentCount    int
	MessageCount  int
	ByConv        string
	Note          string
}

// execer is satisfied by both *sql.DB and *sql.Tx, so a transfer-log
// row can be written either standalone (export) or inside a caller's
// transaction (import — see ImportGroup, where the log row must share
// the import's all-or-nothing fate).
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// insertTransferLog writes one agent_transfer_log row through the given
// execer. Internal helper shared by the exported standalone insert and
// the in-transaction import path.
func insertTransferLog(x execer, e TransferLogEntry) (int64, error) {
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	res, err := x.Exec(`
		INSERT INTO agent_transfer_log
			(kind, at, format_version, source_group, source_home, source_os,
			 result_group, target_dir, conv_remaps, agent_count, message_count,
			 by_conv, note, by_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+agentForConvExpr+`)`,
		e.Kind, at.Format(time.RFC3339Nano), e.FormatVersion,
		e.SourceGroup, e.SourceHome, e.SourceOS,
		e.ResultGroup, e.TargetDir, e.ConvRemaps,
		e.AgentCount, e.MessageCount, e.ByConv, e.Note, e.ByConv)
	if err != nil {
		return 0, fmt.Errorf("insert transfer log: %w", err)
	}
	return res.LastInsertId()
}

// InsertTransferLog records one export/import in agent_transfer_log.
// Used by the export path (a plain, non-transactional write — an export
// mutates nothing else, so a logging failure must not fail the export;
// callers treat the error as best-effort). The import path does NOT use
// this: it logs through insertTransferLog inside its own transaction so
// a rolled-back import leaves no log row.
func InsertTransferLog(e TransferLogEntry) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	return insertTransferLog(d, e)
}

// ListTransferLog returns the most recent transfer-log entries, newest
// first. limit <= 0 means no limit.
func ListTransferLog(limit int) ([]TransferLogEntry, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	query := `
		SELECT id, kind, at, format_version, source_group, source_home,
		       source_os, result_group, target_dir, conv_remaps,
		       agent_count, message_count, by_conv, note
		FROM agent_transfer_log
		ORDER BY id DESC`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := d.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []TransferLogEntry
	for rows.Next() {
		var e TransferLogEntry
		var at string
		if err := rows.Scan(&e.ID, &e.Kind, &at, &e.FormatVersion,
			&e.SourceGroup, &e.SourceHome, &e.SourceOS, &e.ResultGroup,
			&e.TargetDir, &e.ConvRemaps, &e.AgentCount, &e.MessageCount,
			&e.ByConv, &e.Note); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
			e.At = t
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
