package harness

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// The fixtures here reproduce the REAL 1.0.77 DDL, verbatim down to the
// nullability and the column order, because that is what the reader's contract
// is written against. A fixture that "looked close enough" would let a genuine
// mismatch pass — the whole point of the schema probe is to notice exactly this
// kind of drift.

const copilotUsageFixtureDDL = `
CREATE TABLE schema_version (version INTEGER NOT NULL);
CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	cwd TEXT,
	repository TEXT,
	host_type TEXT,
	branch TEXT,
	summary TEXT,
	created_at TEXT DEFAULT (datetime('now')),
	updated_at TEXT DEFAULT (datetime('now')));
CREATE TABLE assistant_usage_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL REFERENCES sessions(id),
	turn_index INTEGER,
	agent_id TEXT,
	parent_tool_call_id TEXT,
	model TEXT NOT NULL,
	input_tokens INTEGER,
	output_tokens INTEGER,
	cache_read_tokens INTEGER,
	cache_write_tokens INTEGER,
	reasoning_tokens INTEGER,
	total_nano_aiu INTEGER,
	request_multiplier REAL,
	duration_ms INTEGER,
	time_to_first_token_ms INTEGER,
	inter_token_latency_ms INTEGER,
	initiator TEXT,
	api_endpoint TEXT,
	reasoning_effort TEXT,
	finish_reason TEXT,
	content_filter_triggered INTEGER,
	token_details_json TEXT,
	created_at TEXT DEFAULT (datetime('now')));
CREATE INDEX idx_assistant_usage_events_session
	ON assistant_usage_events(session_id, id);
INSERT INTO schema_version (version) VALUES (6);
`

// copilotUsageFixtureCall is one row to seed. Only the fields the tests care
// about are named; the rest are left at Copilot's own defaults or NULL, which
// is itself worth exercising — every numeric column in the real DDL is
// nullable.
type copilotUsageFixtureCall struct {
	sessionID    string
	turnIndex    int64
	model        string
	input        int64
	output       int64
	cacheRead    int64
	cacheWrite   int64
	reasoning    int64
	nanoAIU      any
	multiplier   float64
	durationMs   int64
	effort       string
	finishReason string
	tokenDetails string
}

// newCopilotUsageFixture builds a store at a temp COPILOT_HOME and returns the
// home. `wal` chooses the journal mode: the live store runs WAL, but the
// busy-contention test needs a rollback journal, where a writer's exclusive
// lock actually blocks a reader.
func newCopilotUsageFixture(t *testing.T, wal bool, calls ...copilotUsageFixtureCall) string {
	t.Helper()
	home := t.TempDir()
	writer := openCopilotUsageFixtureWriter(t, home, wal)
	seedCopilotUsageFixture(t, writer, calls...)
	require.NoError(t, writer.Close())
	return home
}

func openCopilotUsageFixtureWriter(t *testing.T, home string, wal bool) *sql.DB {
	t.Helper()
	path := filepath.Join(home, copilotUsageStoreFileName)
	writer, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err, "open fixture writer")
	mode := "DELETE"
	if wal {
		mode = "WAL"
	}
	_, err = writer.Exec("PRAGMA journal_mode=" + mode)
	require.NoError(t, err, "set journal mode")
	_, err = writer.Exec(copilotUsageFixtureDDL)
	require.NoError(t, err, "create fixture schema")
	return writer
}

func seedCopilotUsageFixture(t *testing.T, writer *sql.DB, calls ...copilotUsageFixtureCall) {
	t.Helper()
	for _, call := range calls {
		_, err := writer.Exec(
			`INSERT OR IGNORE INTO sessions (id) VALUES (?)`, call.sessionID)
		require.NoError(t, err, "seed session")
		_, err = writer.Exec(`INSERT INTO assistant_usage_events
			(session_id, turn_index, model, input_tokens, output_tokens,
			 cache_read_tokens, cache_write_tokens, reasoning_tokens,
			 total_nano_aiu, request_multiplier, duration_ms,
			 reasoning_effort, finish_reason, token_details_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			call.sessionID, call.turnIndex, call.model, call.input, call.output,
			call.cacheRead, call.cacheWrite, call.reasoning, call.nanoAIU,
			call.multiplier, call.durationMs, call.effort, call.finishReason,
			call.tokenDetails, "2026-08-04T12:00:00Z")
		require.NoError(t, err, "seed usage row")
	}
}

func TestCopilotUsageStoreReadsFreshRowsAcrossSessions(t *testing.T) {
	home := newCopilotUsageFixture(t, true,
		copilotUsageFixtureCall{sessionID: "sess-a", turnIndex: 0, model: "gpt-5",
			input: 25114, output: 300, cacheRead: 23040, nanoAIU: int64(1200),
			multiplier: 1, durationMs: 900, effort: "medium", finishReason: "stop"},
		copilotUsageFixtureCall{sessionID: "sess-b", turnIndex: 0, model: "claude",
			input: 500, output: 40},
		copilotUsageFixtureCall{sessionID: "sess-a", turnIndex: 1, model: "gpt-5",
			input: 28725, output: 700, cacheRead: 25111, cacheWrite: 3611,
			nanoAIU: int64(4000), finishReason: "tool_calls"},
	)
	store, err := OpenCopilotUsageStore(home)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	assert.Equal(t, int64(6), store.SchemaVersion())

	calls, err := store.Calls(context.Background(), []CopilotUsageCursor{
		{SessionID: "sess-a"}, {SessionID: "sess-b"},
	}, 100)
	require.NoError(t, err)
	require.Len(t, calls, 3, "one batched query returns every session's rows")

	// Ordered by (session_id, id), so sess-a's two calls come first and in
	// ascending event id — the order the agentd fold depends on.
	assert.Equal(t, "sess-a", calls[0].SessionID)
	assert.Equal(t, "sess-a", calls[1].SessionID)
	assert.Equal(t, "sess-b", calls[2].SessionID)
	assert.Less(t, calls[0].EventID, calls[1].EventID)

	assert.Equal(t, int64(25114), calls[0].InputTokens)
	assert.Equal(t, int64(23040), calls[0].CacheReadTokens)
	assert.Equal(t, "medium", calls[0].ReasoningEffort)
	assert.Equal(t, "stop", calls[0].FinishReason)
	assert.Equal(t, int64(900), calls[0].DurationMs)
	assert.Equal(t, int64(1200), calls[0].TotalNanoAIU)
	assert.True(t, calls[0].HasNanoAIU)
	assert.Equal(t, "2026-08-04T12:00:00Z", calls[0].CreatedAt)

	// A NULL total_nano_aiu must read as "Copilot said nothing", not as a
	// reported zero — the same distinction the durable follower draws.
	assert.False(t, calls[2].HasNanoAIU)
	assert.Zero(t, calls[2].TotalNanoAIU)
}

// TestCopilotUsageContextNumeratorIsInputTokensAlone pins the one decision the
// live context meter rests on.
//
// The operator reconciled token_details_json's per-type counts against
// input_tokens on the real store and they sum EXACTLY: 25114 = 2074 fresh +
// 23040 cache_read, and 28725 = 3 fresh + 25111 cache_read + 3611 cache_write.
// input_tokens is therefore the whole prompt already, and adding the cache
// columns to it would double-count. The fixtures below carry those exact
// numbers so the reconciliation is checkable here rather than only asserted in
// a comment.
func TestCopilotUsageContextNumeratorIsInputTokensAlone(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		input, cacheRead, cacheWrite int64
		freshTokens                  int64
	}{
		{"cached prefix only", 25114, 23040, 0, 2074},
		{"cached prefix plus fresh cache write", 28725, 25111, 3611, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The reconciliation itself: the per-type breakdown sums to
			// input_tokens, which is why input_tokens alone is the occupancy.
			require.Equal(t, tc.input, tc.freshTokens+tc.cacheRead+tc.cacheWrite,
				"token_details breakdown must sum to input_tokens")

			call := CopilotUsageCall{
				InputTokens:      tc.input,
				CacheReadTokens:  tc.cacheRead,
				CacheWriteTokens: tc.cacheWrite,
				OutputTokens:     999,
				ReasoningTokens:  111,
			}
			assert.Equal(t, tc.input, call.ContextTokens(),
				"occupancy is the prompt, not the prompt plus its own breakdown")
			assert.NotEqual(t, tc.input+tc.cacheRead+tc.cacheWrite, call.ContextTokens(),
				"summing the cache columns onto input_tokens would double-count")
		})
	}
}

func TestCopilotUsageStoreResumesFromCursor(t *testing.T) {
	home := newCopilotUsageFixture(t, true,
		copilotUsageFixtureCall{sessionID: "sess-a", model: "m", input: 10},
		copilotUsageFixtureCall{sessionID: "sess-a", model: "m", input: 20},
		copilotUsageFixtureCall{sessionID: "sess-b", model: "m", input: 30},
	)
	store, err := OpenCopilotUsageStore(home)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	first, err := store.Calls(context.Background(), []CopilotUsageCursor{
		{SessionID: "sess-a"}, {SessionID: "sess-b"},
	}, 100)
	require.NoError(t, err)
	require.Len(t, first, 3)

	// Cursors are INDEPENDENT: sess-a is advanced past everything it has while
	// sess-b stays at zero. A shared floor would either re-deliver sess-b's row
	// or hide it.
	resumed, err := store.Calls(context.Background(), []CopilotUsageCursor{
		{SessionID: "sess-a", AfterEventID: first[1].EventID},
		{SessionID: "sess-b"},
	}, 100)
	require.NoError(t, err)
	require.Len(t, resumed, 1, "only the un-consumed session's row comes back")
	assert.Equal(t, "sess-b", resumed[0].SessionID)

	// Fully caught up: nothing at all, which is the steady state between turns.
	drained, err := store.Calls(context.Background(), []CopilotUsageCursor{
		{SessionID: "sess-a", AfterEventID: first[1].EventID},
		{SessionID: "sess-b", AfterEventID: resumed[0].EventID},
	}, 100)
	require.NoError(t, err)
	assert.Empty(t, drained)
}

// TestCopilotUsageStoreSeesRowsAppendedAfterOpen is the property that makes
// mode=ro (rather than immutable=1) the right choice: the store is written
// while tclaude holds it open, and an immutable handle would serve a stale view.
func TestCopilotUsageStoreSeesRowsAppendedAfterOpen(t *testing.T) {
	home := newCopilotUsageFixture(t, true,
		copilotUsageFixtureCall{sessionID: "sess-a", model: "m", input: 10})
	store, err := OpenCopilotUsageStore(home)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	first, err := store.Calls(context.Background(),
		[]CopilotUsageCursor{{SessionID: "sess-a"}}, 100)
	require.NoError(t, err)
	require.Len(t, first, 1)

	writer := openCopilotUsageFixtureWriterExisting(t, home)
	seedCopilotUsageFixture(t, writer, copilotUsageFixtureCall{
		sessionID: "sess-a", model: "m", input: 99})
	require.NoError(t, writer.Close())

	next, err := store.Calls(context.Background(),
		[]CopilotUsageCursor{{SessionID: "sess-a", AfterEventID: first[0].EventID}}, 100)
	require.NoError(t, err)
	require.Len(t, next, 1, "an already-open read-only handle must see live appends")
	assert.Equal(t, int64(99), next[0].InputTokens)
}

func openCopilotUsageFixtureWriterExisting(t *testing.T, home string) *sql.DB {
	t.Helper()
	writer, err := sql.Open("sqlite", "file:"+filepath.Join(home, copilotUsageStoreFileName))
	require.NoError(t, err)
	return writer
}

func TestCopilotUsageStoreLimitDrainsAcrossCalls(t *testing.T) {
	var seed []copilotUsageFixtureCall
	for i := range 5 {
		seed = append(seed, copilotUsageFixtureCall{
			sessionID: "sess-a", model: "m", input: int64(i + 1)})
	}
	home := newCopilotUsageFixture(t, true, seed...)
	store, err := OpenCopilotUsageStore(home)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	// Hitting the cap is normal, not an error: it is how a first sight of a
	// long-running session drains over several sweeps.
	batch, err := store.Calls(context.Background(),
		[]CopilotUsageCursor{{SessionID: "sess-a"}}, 2)
	require.NoError(t, err)
	require.Len(t, batch, 2)
	assert.Equal(t, int64(1), batch[0].InputTokens)

	rest, err := store.Calls(context.Background(),
		[]CopilotUsageCursor{{SessionID: "sess-a", AfterEventID: batch[1].EventID}}, 100)
	require.NoError(t, err)
	require.Len(t, rest, 3, "the remainder arrives on the next sweep, nothing skipped")
	assert.Equal(t, int64(3), rest[0].InputTokens)
}

func TestCopilotUsageStoreIsReadOnly(t *testing.T) {
	home := newCopilotUsageFixture(t, true,
		copilotUsageFixtureCall{sessionID: "sess-a", model: "m", input: 1})
	store, err := OpenCopilotUsageStore(home)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	// Reaching through the handle to write must fail. This asserts the DSN's
	// mode=ro + query_only(1) actually took effect, rather than trusting that
	// the reader's own SELECTs happen to be the only statements it issues.
	for _, statement := range []string{
		`INSERT INTO assistant_usage_events (session_id, model) VALUES ('x', 'm')`,
		`DELETE FROM assistant_usage_events`,
		`UPDATE assistant_usage_events SET input_tokens = 0`,
		`CREATE TABLE tclaude_should_never_exist (x INTEGER)`,
		`DROP TABLE assistant_usage_events`,
	} {
		_, err := store.db.Exec(statement)
		assert.Error(t, err, "read-only store must refuse: %s", statement)
	}
}

// TestCopilotUsageStoreNeverCheckpointsTheWAL is separate from the write test
// because a checkpoint is not refused the same way a write is — SQLite reports
// a busy/no-op result rather than an error. What must hold is the EFFECT: a
// live WAL owned by Copilot's process must still be there afterwards.
//
// Truncating someone else's WAL is not tclaude's business, and it is the one
// destructive thing a "read-only" reader could still plausibly do to a database
// it does not own.
func TestCopilotUsageStoreNeverCheckpointsTheWAL(t *testing.T) {
	home := newCopilotUsageFixture(t, true,
		copilotUsageFixtureCall{sessionID: "sess-a", model: "m", input: 1})

	// A writer left OPEN, so the -wal is live and unconsolidated — the state the
	// store is actually in while a Copilot pane is running.
	writer := openCopilotUsageFixtureWriterExisting(t, home)
	t.Cleanup(func() { _ = writer.Close() })
	_, err := writer.Exec(`PRAGMA journal_mode=WAL`)
	require.NoError(t, err)
	seedCopilotUsageFixture(t, writer,
		copilotUsageFixtureCall{sessionID: "sess-a", model: "m", input: 2})

	walPath := filepath.Join(home, copilotUsageStoreFileName+"-wal")
	before, err := os.Stat(walPath)
	require.NoError(t, err, "fixture must have a live WAL to protect")
	require.Positive(t, before.Size())

	store, err := OpenCopilotUsageStore(home)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	// Even asked directly, the read-only handle must not shrink the WAL.
	_, _ = store.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)

	after, err := os.Stat(walPath)
	require.NoError(t, err, "the WAL must still exist")
	assert.Equal(t, before.Size(), after.Size(),
		"a read-only reader must never checkpoint Copilot's WAL")
}

func TestCopilotUsageStoreAbsent(t *testing.T) {
	_, err := OpenCopilotUsageStore(t.TempDir())
	assert.ErrorIs(t, err, ErrCopilotUsageStoreAbsent,
		"a host without Copilot is the ordinary case, not a fault")

	_, err = OpenCopilotUsageStore("")
	assert.ErrorIs(t, err, ErrCopilotUsageStoreAbsent,
		"an unresolvable COPILOT_HOME must not be opened as a relative path")
}

// TestCopilotUsageStoreDegradesOnSchemaDrift covers the "a Copilot release
// renamed something" case. Every variant must be refused at OPEN, before any
// row-shaped assumption is made, and must not be ErrCopilotUsageStoreAbsent —
// the caller warns on drift and stays silent on absence.
func TestCopilotUsageStoreDegradesOnSchemaDrift(t *testing.T) {
	for _, tc := range []struct {
		name, ddl, wantSubstring string
	}{
		{
			name: "missing usage table",
			ddl: `CREATE TABLE schema_version (version INTEGER NOT NULL);
				INSERT INTO schema_version (version) VALUES (7);`,
			wantSubstring: "no assistant_usage_events table",
		},
		{
			name: "renamed column",
			ddl: `CREATE TABLE schema_version (version INTEGER NOT NULL);
				CREATE TABLE assistant_usage_events (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					session_id TEXT NOT NULL,
					turn_index INTEGER,
					model TEXT NOT NULL,
					prompt_tokens INTEGER,
					output_tokens INTEGER,
					cache_read_tokens INTEGER,
					cache_write_tokens INTEGER,
					reasoning_tokens INTEGER,
					total_nano_aiu INTEGER,
					request_multiplier REAL,
					duration_ms INTEGER,
					time_to_first_token_ms INTEGER,
					inter_token_latency_ms INTEGER,
					reasoning_effort TEXT,
					finish_reason TEXT,
					created_at TEXT);
				INSERT INTO schema_version (version) VALUES (7);`,
			wantSubstring: "missing input_tokens",
		},
		{
			name:          "not copilot's store at all",
			ddl:           `CREATE TABLE something_else (x INTEGER);`,
			wantSubstring: "read schema_version",
		},
		{
			name:          "empty schema_version",
			ddl:           `CREATE TABLE schema_version (version INTEGER NOT NULL);`,
			wantSubstring: "empty schema_version",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			writer, err := sql.Open("sqlite",
				"file:"+filepath.Join(home, copilotUsageStoreFileName))
			require.NoError(t, err)
			_, err = writer.Exec(tc.ddl)
			require.NoError(t, err)
			require.NoError(t, writer.Close())

			store, err := OpenCopilotUsageStore(home)
			require.Error(t, err, "drift must be refused at open")
			assert.Nil(t, store)
			assert.NotErrorIs(t, err, ErrCopilotUsageStoreAbsent,
				"drift is a warnable condition, absence is not")
			assert.Contains(t, err.Error(), tc.wantSubstring)
		})
	}
}

// TestCopilotUsageStoreNewerSchemaVersionStillReadable pins the
// newer-version-usable-if-columns-present rule: a Copilot release that bumps
// its schema without touching the fields tclaude contracts must not blank the
// meter.
func TestCopilotUsageStoreNewerSchemaVersionStillReadable(t *testing.T) {
	home := newCopilotUsageFixture(t, true,
		copilotUsageFixtureCall{sessionID: "sess-a", model: "m", input: 42})
	writer := openCopilotUsageFixtureWriterExisting(t, home)
	_, err := writer.Exec(`UPDATE schema_version SET version = 9999;
		ALTER TABLE assistant_usage_events ADD COLUMN some_future_field TEXT`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	store, err := OpenCopilotUsageStore(home)
	require.NoError(t, err, "an unknown-but-compatible version stays readable")
	t.Cleanup(func() { _ = store.Close() })
	assert.Equal(t, int64(9999), store.SchemaVersion())

	calls, err := store.Calls(context.Background(),
		[]CopilotUsageCursor{{SessionID: "sess-a"}}, 10)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, int64(42), calls[0].InputTokens)
}

// TestCopilotUsageStoreToleratesBusyWriter exercises the SQLITE_BUSY path.
//
// A rollback-journal fixture is used deliberately: in WAL mode a reader does
// not block behind a writer at all, so a WAL fixture would assert nothing. What
// matters is that contention surfaces as an ordinary error the sweep can skip
// and retry, never a panic or a corrupt partial read.
func TestCopilotUsageStoreToleratesBusyWriter(t *testing.T) {
	home := newCopilotUsageFixture(t, false,
		copilotUsageFixtureCall{sessionID: "sess-a", model: "m", input: 7})
	store, err := OpenCopilotUsageStore(home)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	writer := openCopilotUsageFixtureWriterExisting(t, home)
	t.Cleanup(func() { _ = writer.Close() })
	conn, err := writer.Conn(context.Background())
	require.NoError(t, err)
	_, err = conn.ExecContext(context.Background(), `BEGIN EXCLUSIVE`)
	require.NoError(t, err)

	_, readErr := store.Calls(context.Background(),
		[]CopilotUsageCursor{{SessionID: "sess-a"}}, 10)
	// The exclusive lock must produce a plain error, not a hang past the
	// bounded busy_timeout and not a partial result.
	require.Error(t, readErr)

	_, err = conn.ExecContext(context.Background(), `ROLLBACK`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	// And the very next sweep recovers with nothing lost: the cursor never
	// advanced past a row it failed to read.
	calls, err := store.Calls(context.Background(),
		[]CopilotUsageCursor{{SessionID: "sess-a"}}, 10)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, int64(7), calls[0].InputTokens)
}

// TestCopilotUsageStoreDegradesOnUnwritableWALDirectory pins the caveat called
// out in the design: a read-only SQLite connection to a WAL database needs to
// create or attach the -shm file, so a store whose directory the daemon cannot
// write may be unreadable even though the daemon can read every byte of it.
//
// This is a real operator-visible case (a locked-down COPILOT_HOME), and what
// matters is that it degrades — an error the sweep backs off on — rather than
// producing a wrong reading or taking the daemon down.
func TestCopilotUsageStoreDegradesOnUnwritableWALDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so the case cannot be reproduced")
	}
	home := t.TempDir()
	writer := openCopilotUsageFixtureWriter(t, home, true)
	seedCopilotUsageFixture(t, writer,
		copilotUsageFixtureCall{sessionID: "sess-a", model: "m", input: 5})
	// Leave the WAL in place rather than closing cleanly, which is the live
	// shape: a checkpointed-and-closed store has no -wal to attach.
	_, err := writer.Exec(`PRAGMA wal_checkpoint(PASSIVE)`)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	require.FileExists(t, filepath.Join(home, copilotUsageStoreFileName))

	require.NoError(t, os.Chmod(home, 0o555))
	t.Cleanup(func() { _ = os.Chmod(home, 0o755) })

	store, err := OpenCopilotUsageStore(home)
	if err != nil {
		assert.NotErrorIs(t, err, ErrCopilotUsageStoreAbsent)
		return
	}
	// Some builds attach an existing -shm successfully; then a read must simply
	// work. Either outcome is acceptable — what must never happen is a panic or
	// a silently wrong answer.
	t.Cleanup(func() { _ = store.Close() })
	calls, callErr := store.Calls(context.Background(),
		[]CopilotUsageCursor{{SessionID: "sess-a"}}, 10)
	if callErr == nil {
		require.Len(t, calls, 1)
		assert.Equal(t, int64(5), calls[0].InputTokens)
	}
}

func TestCopilotUsageStoreIgnoresEmptyCursors(t *testing.T) {
	home := newCopilotUsageFixture(t, true,
		copilotUsageFixtureCall{sessionID: "sess-a", model: "m", input: 1})
	store, err := OpenCopilotUsageStore(home)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	calls, err := store.Calls(context.Background(), nil, 10)
	require.NoError(t, err)
	assert.Empty(t, calls)

	calls, err = store.Calls(context.Background(),
		[]CopilotUsageCursor{{SessionID: "   "}}, 10)
	require.NoError(t, err)
	assert.Empty(t, calls, "a blank session id must not widen the predicate")

	calls, err = store.Calls(context.Background(),
		[]CopilotUsageCursor{{SessionID: "sess-a"}}, 0)
	require.NoError(t, err)
	assert.Empty(t, calls, "a zero limit reads nothing rather than everything")
}

// TestCopilotUsageStoreBatchesBeyondOneChunk covers the chunking that keeps the
// statement under SQLite's host-parameter limit. Two parameters per session
// means a single un-chunked statement would need 600 for this fixture.
func TestCopilotUsageStoreBatchesBeyondOneChunk(t *testing.T) {
	const sessions = 300
	var seed []copilotUsageFixtureCall
	for i := range sessions {
		seed = append(seed, copilotUsageFixtureCall{
			sessionID: "sess-" + string(rune('a'+i%26)) + "-" + itoa(i),
			model:     "m", input: int64(i + 1)})
	}
	home := newCopilotUsageFixture(t, true, seed...)
	store, err := OpenCopilotUsageStore(home)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	cursors := make([]CopilotUsageCursor, 0, sessions)
	for _, call := range seed {
		cursors = append(cursors, CopilotUsageCursor{SessionID: call.sessionID})
	}
	calls, err := store.Calls(context.Background(), cursors, 10_000)
	require.NoError(t, err)
	assert.Len(t, calls, sessions, "chunking must not drop or duplicate a session")
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func TestCopilotUsageStorePathEmptyHome(t *testing.T) {
	assert.Empty(t, CopilotUsageStorePath(""))
	assert.Empty(t, CopilotUsageStorePath("   "))
	assert.Equal(t, filepath.Join("/tmp/home", "session-store.db"),
		CopilotUsageStorePath("/tmp/home"))
}

func TestCopilotUsageStoreRefusesNonRegularFile(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(home, copilotUsageStoreFileName), 0o755))
	_, err := OpenCopilotUsageStore(home)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a regular file")
	assert.False(t, errors.Is(err, ErrCopilotUsageStoreAbsent))
}
