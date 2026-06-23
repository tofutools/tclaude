package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Audit actor kinds stored in audit_log.actor_kind.
const (
	AuditActorHuman   = "human"   // the operator — dashboard, or a CLI caller with a valid operator token
	AuditActorAgent   = "agent"   // a confirmed harness caller with a resolved conv-id
	AuditActorUnknown = "unknown" // identity could not be resolved (fail-closed callers still get logged)
)

// Audit sources stored in audit_log.source — which daemon surface the
// command arrived on. v1 captures only daemon-proxied commands, but the
// surface (the `tclaude agent` CLI vs the browser dashboard) is recorded
// so the trail reads correctly and a future "also direct CLI" expansion
// has a slot to tag.
const (
	AuditSourceCLI       = "cli"       // /v1/* over the unix socket (the `tclaude agent` CLI)
	AuditSourceDashboard = "dashboard" // /api/* on the loopback dashboard server
)

// AuditLogEntry is one row of audit_log — the persistent trail of
// daemon-proxied tclaude commands. It records WHO ran WHAT against WHICH
// target, with both a symbolic representation (actor/verb/target/detail)
// and the raw HTTP (method/path/status) for debugging and for the
// generic case where no symbolic describer exists.
//
// Actor and target *labels* are denormalized snapshots taken at record
// time so a row stays readable after the agent it names is renamed,
// retired, or deleted.
type AuditLogEntry struct {
	ID          int64
	At          time.Time
	ActorKind   string // AuditActor*
	ActorConv   string // conv-id when ActorKind == agent; empty for human
	ActorLabel  string // display-title snapshot of the actor
	Verb        string // symbolic verb: spawn, message, reincarnate, rename, retire, delete, cron.add, …
	TargetConv  string // target conv-id when applicable
	TargetLabel string // display-title snapshot of the target
	GroupName   string // group context when applicable
	Detail      string // symbolic detail: message preview, new title, slug, cron body preview, …
	Method      string // raw HTTP method
	Path        string // raw HTTP path
	Status      int    // HTTP status; >= 400 means the command was denied or errored
	Source      string // AuditSource*
}

// auditExecer is satisfied by both *sql.DB and *sql.Tx so a row can be
// written standalone or inside a caller's transaction.
type auditExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertAuditLog(x auditExecer, e AuditLogEntry) (int64, error) {
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	res, err := x.Exec(`
		INSERT INTO audit_log
			(at, actor_kind, actor_conv, actor_label, verb,
			 target_conv, target_label, group_name, detail,
			 method, path, status, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		at.UTC().Format(time.RFC3339Nano), e.ActorKind, e.ActorConv, e.ActorLabel, e.Verb,
		e.TargetConv, e.TargetLabel, e.GroupName, e.Detail,
		e.Method, e.Path, e.Status, e.Source)
	if err != nil {
		return 0, fmt.Errorf("insert audit log: %w", err)
	}
	return res.LastInsertId()
}

// InsertAuditLog records one audit row. Callers treat the error as
// best-effort: a logging failure must never fail the underlying command.
func InsertAuditLog(e AuditLogEntry) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	return insertAuditLog(d, e)
}

// AuditLogFilter narrows ListAuditLog. Zero-valued fields are ignored.
type AuditLogFilter struct {
	Limit  int    // max rows (newest first); <= 0 means a default cap is applied
	Verb   string // exact-match on verb
	Source string // exact-match on source
	// Outcome filters the status class:
	//   ""        — all rows
	//   "success" — status < 400
	//   "failure" — status >= 400 (denials + errors)
	Outcome string
}

// DefaultAuditLogLimit caps an unbounded ListAuditLog so a chatty group
// can't ship an unbounded result set to the dashboard in one shot.
const DefaultAuditLogLimit = 1000

// ListAuditLog returns audit rows newest-first. Ordering is by id (NOT
// at): at is RFC3339Nano TEXT, whose lexical order misorders rows that
// share a whole-second timestamp — a known hazard in this DB. id is
// monotonic with insert order, so it is the correct newest-first key.
func ListAuditLog(f AuditLogFilter) ([]AuditLogEntry, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}

	var where []string
	var args []any
	if f.Verb != "" {
		where = append(where, "verb = ?")
		args = append(args, f.Verb)
	}
	if f.Source != "" {
		where = append(where, "source = ?")
		args = append(args, f.Source)
	}
	switch f.Outcome {
	case "success":
		where = append(where, "status < 400")
	case "failure":
		where = append(where, "status >= 400")
	}

	query := `
		SELECT id, at, actor_kind, actor_conv, actor_label, verb,
		       target_conv, target_label, group_name, detail,
		       method, path, status, source
		FROM audit_log`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id DESC"
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultAuditLogLimit
	}
	query += " LIMIT ?"
	args = append(args, limit)

	rows, err := d.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []AuditLogEntry
	for rows.Next() {
		var e AuditLogEntry
		var at string
		if err := rows.Scan(&e.ID, &at, &e.ActorKind, &e.ActorConv, &e.ActorLabel, &e.Verb,
			&e.TargetConv, &e.TargetLabel, &e.GroupName, &e.Detail,
			&e.Method, &e.Path, &e.Status, &e.Source); err != nil {
			return nil, err
		}
		if t, perr := time.Parse(time.RFC3339Nano, at); perr == nil {
			e.At = t
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneAuditLog deletes rows older than cutoff and returns the number
// removed. Used by the daemon's periodic cleanup to enforce the
// configurable retention window. The cutoff is compared against the
// RFC3339Nano `at` text: a `<` comparison against a far-away cutoff is
// unaffected by the sub-second lexical-misorder hazard (that only
// reshuffles rows within the same whole second), so this is safe.
func PruneAuditLog(cutoff time.Time) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`DELETE FROM audit_log WHERE at < ?`,
		cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("prune audit log: %w", err)
	}
	return res.RowsAffected()
}

// CountAuditLog returns the total number of audit rows. Used by tests
// and the prune/retention diagnostics.
func CountAuditLog() (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	var n int64
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
