package db

import (
	"database/sql"
	"fmt"
)

// migrateV187toV188 adds the Copilot live-usage snapshot.
//
// Copilot is the one supported harness whose durable event log cannot produce a
// live token meter: the events that carry per-call usage are flagged
// `ephemeral` and never written to disk. It does persist the same accounting in
// its OWN SQLite store, which agentd polls read-only; this table is where that
// poll lands.
//
// It is deliberately a side table rather than more columns on `sessions`:
//
//   - It is Copilot-only. Widening the shared session row with harness-specific
//     per-call accounting would put columns on every other harness's rows that
//     can never be anything but zero.
//   - It carries the poller's CURSOR (`last_event_id`) as well as its totals,
//     and a cursor is state one writer owns. The rendered figures still go to
//     the shared `sessions` context columns, so the dashboard needs no new read
//     path — this table is the accounting, that row is the display.
//
// ON DELETE CASCADE from sessions: a pruned session must not leave a cursor
// behind that a recreated id could inherit.
func migrateV187toV188(d *sql.DB) error {
	if _, err := d.Exec(`
		CREATE TABLE IF NOT EXISTS copilot_usage_snapshots (
			session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
			conv_id TEXT NOT NULL,
			last_event_id INTEGER NOT NULL CHECK (last_event_id >= 0),
			last_turn_index INTEGER NOT NULL DEFAULT 0,
			model TEXT NOT NULL DEFAULT '',
			reasoning_effort TEXT NOT NULL DEFAULT '',
			finish_reason TEXT NOT NULL DEFAULT '',
			requests INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			cache_write_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			total_nano_aiu INTEGER,
			request_multiplier REAL,
			last_call_input_tokens INTEGER NOT NULL DEFAULT 0,
			last_call_output_tokens INTEGER NOT NULL DEFAULT 0,
			last_call_cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			last_call_cache_write_tokens INTEGER NOT NULL DEFAULT 0,
			last_duration_ms INTEGER NOT NULL DEFAULT 0,
			last_time_to_first_token_ms INTEGER NOT NULL DEFAULT 0,
			last_inter_token_latency_ms INTEGER NOT NULL DEFAULT 0,
			last_call_stamp_text TEXT NOT NULL DEFAULT '',
			observed_at INTEGER NOT NULL
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_copilot_usage_snapshots_conv
			ON copilot_usage_snapshots(conv_id);
		UPDATE schema_version SET version = 188;
	`); err != nil {
		return fmt.Errorf("migrate v187→v188 (Copilot usage snapshots): %w", err)
	}
	return nil
}
