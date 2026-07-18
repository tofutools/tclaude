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
	AuditActorSystem  = "system"  // an internal observer such as a hook or reconciliation sweep
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
	AuditSourcePopup     = "popup"     // /approve/* on the loopback popup server (human approval decisions)
	AuditSourceTmux      = "tmux"      // pane-local pane-exited callback
	AuditSourceHook      = "hook"      // harness lifecycle hook
	AuditSourceReaper    = "reaper"    // steady-state liveness reconciliation
	AuditSourceReconcile = "reconcile" // daemon-start reconciliation of a pre-existing corpse
)

// AuditLogEntry is one row of audit_log — the persistent trail of
// daemon-proxied tclaude commands (JOH-268). It records WHO ran WHAT
// against WHICH target, with both a symbolic representation
// (actor/verb/target/detail) and the raw HTTP (method/path/status) for
// debugging and for the generic case where no symbolic describer exists.
//
// Actor and target *labels* are denormalized snapshots taken at record
// time so a row stays readable after the agent it names is renamed,
// retired, or deleted.
type AuditLogEntry struct {
	ID          int64
	At          time.Time
	ActorKind   string // AuditActor*
	ActorConv   string // conv-id when ActorKind == agent; empty for human
	ActorAgent  string // stable agent_id of the actor (PR4 dual-write); "" for human / non-actor conv
	ActorLabel  string // display-title snapshot of the actor
	Verb        string // symbolic verb: spawn, message, reincarnate, rename, retire, delete, cron.add, …
	TargetConv  string // target conv-id when applicable
	TargetAgent string // stable agent_id of the target (PR4 dual-write); "" when none
	TargetLabel string // display-title snapshot of the target
	GroupName   string // group context when applicable
	Detail      string // symbolic detail: message preview, new title, slug, cron body preview, …
	Method      string // raw HTTP method
	Path        string // raw HTTP path
	Status      int    // HTTP status; >= 400 means the command was denied or errored
	Source      string // AuditSource*

	// Exit-observation fields. They are empty on ordinary command audit rows,
	// keeping the established API/storage shape backwards compatible.
	EventID         string
	RelatedEventID  string
	SessionID       string
	TmuxSession     string
	PaneID          string
	Observer        string
	CauseKind       string
	ExitCode        *int
	Signal          string
	LifecycleAction string
	Reason          string
	ObservedState   string
	DedupKey        string
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
	// actor_agent / target_agent are dual-written from an explicitly captured
	// stable identity when the caller has one, with conv-derived lookup retained
	// for legacy callers. The explicit form is important for delayed audit sinks:
	// the originating generation may have rotated or been unlinked by write time.
	res, err := x.Exec(`
		INSERT INTO audit_log
			(at, actor_kind, actor_conv, actor_label, verb,
			 target_conv, target_label, group_name, detail,
			 method, path, status, source,
			 actor_agent, target_agent,
			 event_id, related_event_id, session_id, tmux_session, pane_id,
			 observer, cause_kind, exit_code, signal, lifecycle_action,
			 reason, observed_state, dedup_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			COALESCE(NULLIF(?, ''), `+agentForConvExpr+`),
			COALESCE(NULLIF(?, ''), `+agentForConvExpr+`),
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		at.UTC().Format(time.RFC3339Nano), e.ActorKind, e.ActorConv, e.ActorLabel, e.Verb,
		e.TargetConv, e.TargetLabel, e.GroupName, e.Detail,
		e.Method, e.Path, e.Status, e.Source,
		e.ActorAgent, e.ActorConv, e.TargetAgent, e.TargetConv,
		e.EventID, e.RelatedEventID, e.SessionID, e.TmuxSession, e.PaneID,
		e.Observer, e.CauseKind, e.ExitCode, e.Signal, e.LifecycleAction,
		e.Reason, e.ObservedState, e.DedupKey)
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

// Audit sort keys (AuditLogFilter.SortBy). The set is a whitelist mapped
// to real columns in auditOrderBy — never interpolate caller input into
// the ORDER BY.
const (
	AuditSortTime   = "time"   // by insert order (id) — the default
	AuditSortActor  = "actor"  // by actor label
	AuditSortVerb   = "verb"   // by verb
	AuditSortTarget = "target" // by target label
	AuditSortStatus = "status" // by HTTP status
)

// AuditLogFilter narrows + orders + paginates ListAuditLog and feeds
// CountAuditLog. Zero-valued fields are ignored.
type AuditLogFilter struct {
	// Filters (all ANDed).
	Verb    string // exact-match on verb
	Source  string // exact-match on source
	Outcome string // "" all; "success" status<400; "failure" status>=400
	Search  string // case-insensitive substring across the symbolic + id columns

	// Sort.
	SortBy string // AuditSort*; "" → AuditSortTime
	Asc    bool   // false (default) → newest/highest first

	// Pagination. Limit <= 0 → DefaultAuditLogLimit. Offset is the number
	// of rows to skip (1-based page → (page-1)*pageSize).
	Limit  int
	Offset int
}

// DefaultAuditLogLimit caps an unbounded ListAuditLog so a chatty group
// can't ship an unbounded result set in one shot.
const DefaultAuditLogLimit = 1000

// auditWhere builds the shared WHERE clause (and its args) from a
// filter's predicate fields. Every value is a bound parameter — no
// caller input is interpolated.
func auditWhere(f AuditLogFilter) (string, []any) {
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
	if s := strings.TrimSpace(f.Search); s != "" {
		// Case-insensitive substring across the symbolic + id columns.
		// % / _ in the term are escaped so they match literally.
		like := "%" + escapeLike(s) + "%"
		cols := []string{"actor_label", "verb", "target_label", "group_name",
			"detail", "actor_conv", "target_conv", "actor_agent", "target_agent",
			"event_id", "related_event_id", "session_id", "tmux_session"}
		var ors []string
		for _, c := range cols {
			ors = append(ors, c+" LIKE ? ESCAPE '\\'")
			args = append(args, like)
		}
		where = append(where, "("+strings.Join(ors, " OR ")+")")
	}
	if len(where) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

// escapeLike escapes the LIKE metacharacters so a user's search term
// matches literally under `ESCAPE '\'`.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// auditOrderBy maps a whitelisted sort key + direction to a safe ORDER
// BY clause. A non-id sort gets a secondary `id` key so ties stay
// stable. Unknown keys fall back to time (id).
func auditOrderBy(sortBy string, asc bool) string {
	dir := "DESC"
	if asc {
		dir = "ASC"
	}
	switch sortBy {
	case AuditSortActor:
		return " ORDER BY actor_label " + dir + ", id DESC"
	case AuditSortVerb:
		return " ORDER BY verb " + dir + ", id DESC"
	case AuditSortTarget:
		return " ORDER BY target_label " + dir + ", id DESC"
	case AuditSortStatus:
		return " ORDER BY status " + dir + ", id DESC"
	default: // AuditSortTime / "" / unknown — id is monotonic with insert order
		return " ORDER BY id " + dir
	}
}

const auditSelectCols = `
	SELECT id, at, actor_kind, actor_conv, actor_agent, actor_label, verb,
	       target_conv, target_agent, target_label, group_name, detail,
	       method, path, status, source,
	       event_id, related_event_id, session_id, tmux_session, pane_id,
	       observer, cause_kind, exit_code, signal, lifecycle_action,
	       reason, observed_state, dedup_key
	FROM audit_log`

// ListAuditLog returns audit rows matching the filter, ordered + paged.
// Ordering defaults to newest-first by id (NOT at: at is RFC3339Nano
// TEXT, whose lexical order misorders rows sharing a whole second — a
// known hazard in this DB; id is monotonic with insert order).
func ListAuditLog(f AuditLogFilter) ([]AuditLogEntry, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}

	whereClause, args := auditWhere(f)
	query := auditSelectCols + whereClause + auditOrderBy(f.SortBy, f.Asc)

	limit := f.Limit
	if limit <= 0 {
		limit = DefaultAuditLogLimit
	}
	query += " LIMIT ?"
	args = append(args, limit)
	if f.Offset > 0 {
		query += " OFFSET ?"
		args = append(args, f.Offset)
	}

	rows, err := d.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []AuditLogEntry
	for rows.Next() {
		var e AuditLogEntry
		var at string
		if err := rows.Scan(&e.ID, &at, &e.ActorKind, &e.ActorConv, &e.ActorAgent, &e.ActorLabel, &e.Verb,
			&e.TargetConv, &e.TargetAgent, &e.TargetLabel, &e.GroupName, &e.Detail,
			&e.Method, &e.Path, &e.Status, &e.Source,
			&e.EventID, &e.RelatedEventID, &e.SessionID, &e.TmuxSession, &e.PaneID,
			&e.Observer, &e.CauseKind, &e.ExitCode, &e.Signal, &e.LifecycleAction,
			&e.Reason, &e.ObservedState, &e.DedupKey); err != nil {
			return nil, err
		}
		if t, perr := time.Parse(time.RFC3339Nano, at); perr == nil {
			e.At = t
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountAuditLog returns the number of rows matching the filter's
// predicates (the sort + pagination fields are ignored). Pass a zero
// AuditLogFilter for the unfiltered total.
func CountAuditLog(f AuditLogFilter) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	whereClause, args := auditWhere(f)
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_log`+whereClause, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
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
