package harness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // read-only access to Copilot's own session store
)

// Copilot's LIVE per-call usage, read from `<COPILOT_HOME>/session-store.db`.
//
// copilot_telemetry.go explains at length why the durable `events.jsonl` can
// never carry a live token meter: the three events that would (`assistant.usage`,
// `session.usage_info`, `model.call_start`) are all flagged `ephemeral` and
// never reach the disk. That conclusion still stands for the event log — but it
// turns out the CLI persists the same per-call accounting somewhere else, in a
// SQLite store it keeps alongside the session-state tree.
//
// Verified against a real 1.0.77 store (schema_version 6):
//
//	CREATE TABLE assistant_usage_events (
//	  id INTEGER PRIMARY KEY AUTOINCREMENT,
//	  session_id TEXT NOT NULL REFERENCES sessions(id),
//	  turn_index INTEGER, agent_id TEXT, parent_tool_call_id TEXT,
//	  model TEXT NOT NULL,
//	  input_tokens INTEGER, output_tokens INTEGER,
//	  cache_read_tokens INTEGER, cache_write_tokens INTEGER,
//	  reasoning_tokens INTEGER,
//	  total_nano_aiu INTEGER, request_multiplier REAL,
//	  duration_ms INTEGER, time_to_first_token_ms INTEGER,
//	  inter_token_latency_ms INTEGER,
//	  initiator TEXT, api_endpoint TEXT,
//	  reasoning_effort TEXT, finish_reason TEXT,
//	  content_filter_triggered INTEGER, token_details_json TEXT,
//	  created_at TEXT DEFAULT (datetime('now')))
//	CREATE INDEX idx_assistant_usage_events_session
//	  ON assistant_usage_events(session_id, id)
//
// `session_id` is the SAME uuid as the `session-state/<id>/` conversation
// directories, so a row joins to a tclaude conversation with no extra mapping,
// and the `(session_id, id)` index makes "everything newer than what I already
// consumed, for these sessions" one range scan per session.
//
// Three properties of this reader are load-bearing and are asserted by tests
// rather than merely intended:
//
//   - It is READ-ONLY on every path. The DSN carries `mode=ro` and, belt and
//     braces, `query_only(1)`, so even a coding mistake cannot write. It never
//     checkpoints: the store is a live WAL database owned by a process tclaude
//     does not control, and truncating its WAL under it is not tclaude's
//     business.
//   - `mode=ro`, NOT `immutable=1`. The store is written while tclaude reads
//     it; immutable would tell SQLite to ignore the WAL and serve a stale (or
//     torn) view of a file that is actively changing.
//   - It reads ONE table. `turns`, `session_files`, `checkpoints`,
//     `dynamic_context_items` and the `search_index*` family are all
//     content-bearing — they hold prompts, tool output and file text — and this
//     reader must never open them. The SELECT list below is likewise explicit
//     rather than `SELECT *`, so a future column carrying content cannot start
//     arriving here because someone widened a query.
//
// `token_details_json` is deliberately NOT read. It carries per-token-type
// nano-AIU `costPerBatch` figures, which is the evidence a USD conversion would
// be built from — and that conversion is a separate ticket with a separate
// question to answer (what an AI unit actually costs). Reading it here would
// put the raw material for a fabricated dollar figure one careless commit away.

// copilotUsageStoreFileName is Copilot's session store, a sibling of the
// session-state/ tree under COPILOT_HOME.
const copilotUsageStoreFileName = "session-store.db"

// copilotUsageEventsTable is the ONLY table this package reads. Named as a
// constant so the "one table" claim above is greppable.
const copilotUsageEventsTable = "assistant_usage_events"

// copilotUsageBusyTimeoutMs bounds how long a read waits behind Copilot's own
// writer before giving up.
//
// Kept short on purpose. This runs on a 2s sweep, so a read that cannot get in
// promptly is better abandoned than queued: the next tick is 2 seconds away and
// the checkpoint means nothing is lost by skipping one.
const copilotUsageBusyTimeoutMs = 250

// copilotUsageRequiredColumns is the schema contract. Every one of these must
// exist before a single row is read; a Copilot release that renames one
// degrades this reader instead of producing wrong numbers or a daemon error.
var copilotUsageRequiredColumns = []string{
	"id", "session_id", "turn_index", "model",
	"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens",
	"reasoning_tokens", "total_nano_aiu", "request_multiplier",
	"duration_ms", "time_to_first_token_ms", "inter_token_latency_ms",
	"reasoning_effort", "finish_reason", "created_at",
}

// CopilotUsageCall is one model call as Copilot accounted for it.
//
// Every field here is one tclaude already contracts elsewhere (tokens, cost
// unit, model, effort, latency, finish reason). Nothing content-bearing has a
// field to land in, which is the same defence CopilotErrorObservation uses
// against `stack`: a field that does not exist cannot be persisted by a later
// change that forgets why.
type CopilotUsageCall struct {
	SessionID string
	// EventID is the store's autoincrement id — the incremental cursor.
	EventID   int64
	TurnIndex int64
	Model     string

	// InputTokens is the FULL prompt for this call.
	//
	// Not a synonym for "fresh tokens": the operator reconciled
	// token_details_json's per-type counts against it on the live store and
	// they sum exactly to this column — 25114 = 2074 fresh + 23040 cache_read,
	// 28725 = 3 fresh + 25111 cache_read + 3611 cache_write. So the cache
	// figures below are a BREAKDOWN of this number, not addends to it, and
	// anything summing them onto it double-counts the prompt.
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64

	// TotalNanoAIU is Copilot's own cost unit, carried as the exact integer the
	// store holds. No USD is derived from it here; see the file comment.
	TotalNanoAIU      int64
	HasNanoAIU        bool
	RequestMultiplier float64

	DurationMs          int64
	TimeToFirstTokenMs  int64
	InterTokenLatencyMs int64

	ReasoningEffort string
	FinishReason    string
	// CreatedAt is Copilot's own timestamp text, retained verbatim rather than
	// parsed: it is used as evidence of freshness, never as a clock tclaude
	// computes with.
	CreatedAt string
}

// ContextTokens is the call's context-window occupancy — the numerator of the
// live context meter.
//
// It is InputTokens alone, for the reason spelled out on that field: the cache
// columns are a breakdown of the prompt, so the whole prompt is already
// counted. This is a named method rather than an inline field read so that the
// one place the meter's numerator is decided is greppable, and so a future
// correction is a one-line change with the evidence attached.
func (c CopilotUsageCall) ContextTokens() int64 { return max(c.InputTokens, 0) }

// CopilotUsageCursor asks for one session's rows newer than what a caller has
// already consumed. AfterEventID of 0 means "everything", which is the correct
// first request for a session tclaude has not seen before.
type CopilotUsageCursor struct {
	SessionID    string
	AfterEventID int64
}

// CopilotUsageStore is a read-only handle on one COPILOT_HOME's session store.
//
// One store serves every conversation under that home, which is the whole point
// of the batched sweep: N conversations cost one connection and one query per
// poll, not N of each.
type CopilotUsageStore struct {
	path string
	db   *sql.DB
	// schemaVersion is Copilot's own `schema_version.version`. It is recorded
	// for diagnostics only — the actual gate is the column probe, because a
	// version number tells you a release changed and not whether the fields
	// this reader needs survived it.
	schemaVersion int64
}

// CopilotUsageStorePath is the store's path under a resolved COPILOT_HOME.
// Empty home yields empty path: callers must treat that as "nothing to read",
// never as a relative path to open.
func CopilotUsageStorePath(home string) string {
	if strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, copilotUsageStoreFileName)
}

// ErrCopilotUsageStoreAbsent reports that this home has no session store —
// Copilot is not installed, has never run, or keeps its state elsewhere.
//
// Distinguished from every other failure because it is the ORDINARY state on
// most hosts and must not be logged as a fault on a 2-second loop.
var ErrCopilotUsageStoreAbsent = errors.New("copilot: no session store")

// OpenCopilotUsageStore opens one home's store read-only and verifies its
// schema before returning it.
//
// The schema check happens HERE rather than at first query so that an
// incompatible store is refused once, at open, and the caller's degrade path
// runs before any row-shaped assumption is made.
func OpenCopilotUsageStore(home string) (*CopilotUsageStore, error) {
	path := CopilotUsageStorePath(home)
	if path == "" {
		return nil, ErrCopilotUsageStoreAbsent
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrCopilotUsageStoreAbsent
	}
	if err != nil {
		return nil, fmt.Errorf("copilot usage store %s: %w", path, err)
	}
	// A symlink or a device node where a database should be is not something to
	// follow on a 2s loop against a path tclaude does not own.
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("copilot usage store %s is not a regular file", path)
	}

	// mode=ro: read-only, but still WAL-aware — see the file comment on why
	// immutable=1 would be wrong here.
	// query_only(1): a second, connection-level refusal of any write.
	// busy_timeout: bounded wait behind Copilot's writer.
	dsn := (&url.URL{
		Scheme: "file",
		Path:   path,
		RawQuery: fmt.Sprintf("mode=ro&_pragma=busy_timeout(%d)&_pragma=query_only(1)",
			copilotUsageBusyTimeoutMs),
	}).String()
	store, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open copilot usage store %s: %w", path, err)
	}
	handle := &CopilotUsageStore{path: path, db: store}
	if err := handle.verifySchema(); err != nil {
		_ = store.Close()
		return nil, err
	}
	return handle, nil
}

func (s *CopilotUsageStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Path is the file this store reads, for diagnostics.
func (s *CopilotUsageStore) Path() string { return s.path }

// SchemaVersion is Copilot's own schema version, for diagnostics.
func (s *CopilotUsageStore) SchemaVersion() int64 { return s.schemaVersion }

// verifySchema refuses anything that is not recognisably Copilot's store with
// the columns this reader needs.
//
// The `schema_version` check is as much an identity check as a version check:
// it is the cheapest way to notice that COPILOT_HOME points somewhere
// unexpected, before any query names a table.
func (s *CopilotUsageStore) verifySchema() error {
	var version int64
	err := s.db.QueryRow(`SELECT version FROM schema_version LIMIT 1`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("copilot usage store %s: empty schema_version", s.path)
	}
	if err != nil {
		return fmt.Errorf("copilot usage store %s: read schema_version: %w", s.path, err)
	}
	s.schemaVersion = version

	present, err := s.columns()
	if err != nil {
		return err
	}
	if len(present) == 0 {
		return fmt.Errorf("copilot usage store %s: no %s table (schema_version %d)",
			s.path, copilotUsageEventsTable, version)
	}
	var missing []string
	for _, column := range copilotUsageRequiredColumns {
		if _, ok := present[column]; !ok {
			missing = append(missing, column)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("copilot usage store %s: %s is missing %s (schema_version %d)",
			s.path, copilotUsageEventsTable, strings.Join(missing, ", "), version)
	}
	return nil
}

// columns returns the usage table's column names, or an empty set when the
// table does not exist at all. pragma_table_info answers both questions with
// one statement and without naming the table in a FROM clause, so a store
// without it produces zero rows rather than an error to unwrap.
func (s *CopilotUsageStore) columns() (map[string]struct{}, error) {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, copilotUsageEventsTable)
	if err != nil {
		return nil, fmt.Errorf("copilot usage store %s: probe columns: %w", s.path, err)
	}
	defer func() { _ = rows.Close() }()
	present := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("copilot usage store %s: probe columns: %w", s.path, err)
		}
		present[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("copilot usage store %s: probe columns: %w", s.path, err)
	}
	return present, nil
}

// copilotUsageCursorChunk bounds how many sessions go into one statement.
//
// SQLite's default host-parameter limit is 999 and each session contributes
// two, so 100 sessions (200 parameters) sits an order of magnitude clear of it
// while still collapsing any realistic number of live panes into one or two
// queries.
const copilotUsageCursorChunk = 100

// copilotUsageSelectColumns is the explicit projection. It exists as a constant
// so that "this reader never selects a content-bearing column" is one place to
// read and one place to review.
const copilotUsageSelectColumns = `session_id, id, turn_index, model,
	input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
	reasoning_tokens, total_nano_aiu, request_multiplier,
	duration_ms, time_to_first_token_ms, inter_token_latency_ms,
	reasoning_effort, finish_reason, created_at`

// Calls returns every row newer than each cursor's checkpoint, oldest first
// within a session, across at most limit rows in total.
//
// The predicate is an OR of `(session_id = ? AND id > ?)` tuples rather than an
// `IN (…) AND id > ?`, because the cursors are INDEPENDENT: one shared floor
// would either re-deliver rows to a session that is ahead or skip rows for one
// that is behind. Written this way, SQLite serves each tuple as its own range
// scan on the `(session_id, id)` index.
//
// Hitting the limit is normal and not an error — it is how a first sight of a
// long-running session drains over several sweeps instead of one long read.
// Callers checkpoint what they got and come back.
func (s *CopilotUsageStore) Calls(
	ctx context.Context,
	cursors []CopilotUsageCursor,
	limit int,
) ([]CopilotUsageCall, error) {
	if s == nil || s.db == nil || len(cursors) == 0 || limit <= 0 {
		return nil, nil
	}
	var calls []CopilotUsageCall
	for start := 0; start < len(cursors) && len(calls) < limit; start += copilotUsageCursorChunk {
		end := min(start+copilotUsageCursorChunk, len(cursors))
		chunk, err := s.callsChunk(ctx, cursors[start:end], limit-len(calls))
		if err != nil {
			return nil, err
		}
		calls = append(calls, chunk...)
	}
	return calls, nil
}

func (s *CopilotUsageStore) callsChunk(
	ctx context.Context,
	cursors []CopilotUsageCursor,
	limit int,
) ([]CopilotUsageCall, error) {
	predicates := make([]string, 0, len(cursors))
	args := make([]any, 0, len(cursors)*2+1)
	for _, cursor := range cursors {
		if strings.TrimSpace(cursor.SessionID) == "" {
			continue
		}
		predicates = append(predicates, "(session_id = ? AND id > ?)")
		args = append(args, cursor.SessionID, max(cursor.AfterEventID, 0))
	}
	if len(predicates) == 0 {
		return nil, nil
	}
	args = append(args, limit)
	query := `SELECT ` + copilotUsageSelectColumns +
		` FROM ` + copilotUsageEventsTable +
		` WHERE ` + strings.Join(predicates, " OR ") +
		` ORDER BY session_id, id LIMIT ?`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("copilot usage store %s: read calls: %w", s.path, err)
	}
	defer func() { _ = rows.Close() }()

	var calls []CopilotUsageCall
	for rows.Next() {
		// Every numeric column but `id` is nullable in Copilot's DDL, so each
		// one is scanned through a Null* and a NULL reads as "not reported"
		// rather than aborting the sweep.
		var (
			call                                                    CopilotUsageCall
			turnIndex, nanoAIU                                      sql.NullInt64
			inputTokens, outputTokens, cacheRead, cacheWrite        sql.NullInt64
			reasoningTokens, durationMs, firstTokenMs, interTokenMs sql.NullInt64
			multiplier                                              sql.NullFloat64
			model, effort, finishReason, createdAt                  sql.NullString
		)
		if err := rows.Scan(
			&call.SessionID, &call.EventID, &turnIndex, &model,
			&inputTokens, &outputTokens, &cacheRead, &cacheWrite,
			&reasoningTokens, &nanoAIU, &multiplier,
			&durationMs, &firstTokenMs, &interTokenMs,
			&effort, &finishReason, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("copilot usage store %s: scan call: %w", s.path, err)
		}
		call.TurnIndex = turnIndex.Int64
		call.Model = strings.TrimSpace(model.String)
		call.InputTokens = max(inputTokens.Int64, 0)
		call.OutputTokens = max(outputTokens.Int64, 0)
		call.CacheReadTokens = max(cacheRead.Int64, 0)
		call.CacheWriteTokens = max(cacheWrite.Int64, 0)
		call.ReasoningTokens = max(reasoningTokens.Int64, 0)
		// HasNanoAIU keeps "Copilot reported zero" distinguishable from
		// "Copilot said nothing", exactly as the durable follower does: a BYOK
		// or mock provider legitimately bills zero.
		if nanoAIU.Valid && nanoAIU.Int64 >= 0 {
			call.TotalNanoAIU = nanoAIU.Int64
			call.HasNanoAIU = true
		}
		call.RequestMultiplier = multiplier.Float64
		call.DurationMs = max(durationMs.Int64, 0)
		call.TimeToFirstTokenMs = max(firstTokenMs.Int64, 0)
		call.InterTokenLatencyMs = max(interTokenMs.Int64, 0)
		call.ReasoningEffort = strings.TrimSpace(effort.String)
		call.FinishReason = strings.TrimSpace(finishReason.String)
		call.CreatedAt = strings.TrimSpace(createdAt.String)
		calls = append(calls, call)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("copilot usage store %s: read calls: %w", s.path, err)
	}
	return calls, nil
}
