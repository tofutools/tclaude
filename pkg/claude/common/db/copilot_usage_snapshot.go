package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Copilot's live per-call usage snapshot — the durable half of the read-only
// poll of Copilot's own session store. See migrate_v188 for why this is a side
// table rather than columns on `sessions`.
//
// Two fields do different jobs and are easy to confuse, so they are named apart:
//
//   - The cumulative columns (InputTokens, OutputTokens, …, Requests) are the
//     sum over every call this poller has consumed for the session. They are
//     session accounting.
//   - The LastCall* columns describe the newest call ALONE. LastCallInputTokens
//     is the live context-window numerator, because Copilot's input_tokens is
//     the full prompt for that call — see harness.CopilotUsageCall.ContextTokens
//     for the evidence.
//
// Summing input_tokens across calls would be a nonsense occupancy (the same
// conversation prefix is re-sent every turn), which is exactly why the two are
// separate columns rather than one field used for both.

// CopilotUsageSnapshot is one session's accumulated live usage plus the
// poller's cursor into Copilot's store.
type CopilotUsageSnapshot struct {
	SessionID string
	ConvID    string
	// FoldVersion identifies the accounting semantics that produced this row.
	// A mismatched version makes the cursor untrustworthy and must be replayed
	// from zero. See migrate_v195 for the old-daemon race this closes.
	FoldVersion int64

	// LastEventID is the poller's checkpoint: the highest
	// assistant_usage_events.id already folded into this row.
	LastEventID   int64
	LastTurnIndex int64

	Model           string
	ReasoningEffort string
	FinishReason    string

	Requests         int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64

	// TotalNanoAIU is the exact sum of Copilot's reported per-call costs. Nil
	// distinguishes "Copilot said nothing" from a legitimate zero (BYOK/mock
	// providers bill zero), matching the durable follower's HasNanoAIU.
	TotalNanoAIU      *int64
	RequestMultiplier *float64

	LastCallInputTokens      int64
	LastCallOutputTokens     int64
	LastCallCacheReadTokens  int64
	LastCallCacheWriteTokens int64

	LastDurationMs          int64
	LastTimeToFirstTokenMs  int64
	LastInterTokenLatencyMs int64
	// LastCallStamp is Copilot's own timestamp text for the newest call, retained
	// verbatim as freshness evidence rather than as a clock tclaude computes
	// with.
	LastCallStamp string

	ObservedAt time.Time
}

// CopilotUsageFoldVersion is the snapshot contract written by this binary.
// Version 1 sums assistant_usage_events.total_nano_aiu as per-call values.
const CopilotUsageFoldVersion int64 = 1

// SaveCopilotUsageSnapshot replaces one session's snapshot, but only while the
// session row is still the generation the caller observed.
//
// The generation guard is the same one the telemetry checkpoint uses and exists
// for the same reason: a session id can be pruned and recreated while the
// daemon's poller state survives, and a cursor from the previous conversation
// would make the new one skip every row Copilot had already written for it.
//
// Reports whether the write landed. False means the generation moved on, which
// is a normal outcome and not an error.
func SaveCopilotUsageSnapshot(snapshot CopilotUsageSnapshot, createdAt time.Time) (bool, error) {
	if strings.TrimSpace(snapshot.SessionID) == "" || strings.TrimSpace(snapshot.ConvID) == "" {
		return false, nil
	}
	if snapshot.LastEventID < 0 {
		return false, fmt.Errorf("copilot usage snapshot: negative cursor %d", snapshot.LastEventID)
	}
	snapshot.FoldVersion = CopilotUsageFoldVersion
	d, err := Open()
	if err != nil {
		return false, err
	}
	observedAt := snapshot.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	result, err := d.Exec(`INSERT INTO copilot_usage_snapshots
			(session_id, conv_id, fold_version, last_event_id, last_turn_index, model,
			 reasoning_effort, finish_reason, requests,
			 input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			 reasoning_tokens, total_nano_aiu, request_multiplier,
			 last_call_input_tokens, last_call_output_tokens,
			 last_call_cache_read_tokens, last_call_cache_write_tokens,
			 last_duration_ms, last_time_to_first_token_ms,
			 last_inter_token_latency_ms, last_call_stamp_text, observed_at)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE EXISTS (
			SELECT 1 FROM sessions WHERE id = ? AND conv_id = ? AND created_at = ?
		)
		ON CONFLICT(session_id) DO UPDATE SET
			conv_id = excluded.conv_id,
			fold_version = excluded.fold_version,
			last_event_id = excluded.last_event_id,
			last_turn_index = excluded.last_turn_index,
			model = excluded.model,
			reasoning_effort = excluded.reasoning_effort,
			finish_reason = excluded.finish_reason,
			requests = excluded.requests,
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			cache_read_tokens = excluded.cache_read_tokens,
			cache_write_tokens = excluded.cache_write_tokens,
			reasoning_tokens = excluded.reasoning_tokens,
			total_nano_aiu = excluded.total_nano_aiu,
			request_multiplier = excluded.request_multiplier,
			last_call_input_tokens = excluded.last_call_input_tokens,
			last_call_output_tokens = excluded.last_call_output_tokens,
			last_call_cache_read_tokens = excluded.last_call_cache_read_tokens,
			last_call_cache_write_tokens = excluded.last_call_cache_write_tokens,
			last_duration_ms = excluded.last_duration_ms,
			last_time_to_first_token_ms = excluded.last_time_to_first_token_ms,
			last_inter_token_latency_ms = excluded.last_inter_token_latency_ms,
			last_call_stamp_text = excluded.last_call_stamp_text,
			observed_at = excluded.observed_at`,
		snapshot.SessionID, snapshot.ConvID, snapshot.FoldVersion,
		snapshot.LastEventID, snapshot.LastTurnIndex,
		snapshot.Model, snapshot.ReasoningEffort, snapshot.FinishReason, snapshot.Requests,
		snapshot.InputTokens, snapshot.OutputTokens, snapshot.CacheReadTokens,
		snapshot.CacheWriteTokens, snapshot.ReasoningTokens,
		nullableInt64(snapshot.TotalNanoAIU), nullableFloat64(snapshot.RequestMultiplier),
		snapshot.LastCallInputTokens, snapshot.LastCallOutputTokens,
		snapshot.LastCallCacheReadTokens, snapshot.LastCallCacheWriteTokens,
		snapshot.LastDurationMs, snapshot.LastTimeToFirstTokenMs,
		snapshot.LastInterTokenLatencyMs, snapshot.LastCallStamp, dbTime(observedAt.UTC()),
		snapshot.SessionID, snapshot.ConvID, dbTime(createdAt))
	if err != nil {
		return false, fmt.Errorf("save copilot usage snapshot: %w", err)
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

// LoadCopilotUsageSnapshot returns one session's snapshot, or nil when the
// poller has never recorded one. A nil result is how a caller learns to start
// from cursor 0 — "no rows consumed yet" — rather than from a guess.
func LoadCopilotUsageSnapshot(sessionID string) (*CopilotUsageSnapshot, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var (
		snapshot   CopilotUsageSnapshot
		nanoAIU    sql.NullInt64
		multiplier sql.NullFloat64
		observedAt int64
	)
	err = d.QueryRow(`SELECT session_id, conv_id, fold_version, last_event_id, last_turn_index, model,
			reasoning_effort, finish_reason, requests,
			input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
			reasoning_tokens, total_nano_aiu, request_multiplier,
			last_call_input_tokens, last_call_output_tokens,
			last_call_cache_read_tokens, last_call_cache_write_tokens,
			last_duration_ms, last_time_to_first_token_ms,
			last_inter_token_latency_ms, last_call_stamp_text, observed_at
		FROM copilot_usage_snapshots WHERE session_id = ?`, sessionID).
		Scan(&snapshot.SessionID, &snapshot.ConvID, &snapshot.FoldVersion, &snapshot.LastEventID,
			&snapshot.LastTurnIndex, &snapshot.Model, &snapshot.ReasoningEffort,
			&snapshot.FinishReason, &snapshot.Requests,
			&snapshot.InputTokens, &snapshot.OutputTokens, &snapshot.CacheReadTokens,
			&snapshot.CacheWriteTokens, &snapshot.ReasoningTokens, &nanoAIU, &multiplier,
			&snapshot.LastCallInputTokens, &snapshot.LastCallOutputTokens,
			&snapshot.LastCallCacheReadTokens, &snapshot.LastCallCacheWriteTokens,
			&snapshot.LastDurationMs, &snapshot.LastTimeToFirstTokenMs,
			&snapshot.LastInterTokenLatencyMs, &snapshot.LastCallStamp, &observedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load copilot usage snapshot: %w", err)
	}
	if nanoAIU.Valid {
		value := nanoAIU.Int64
		snapshot.TotalNanoAIU = &value
	}
	if multiplier.Valid {
		value := multiplier.Float64
		snapshot.RequestMultiplier = &value
	}
	snapshot.ObservedAt = time.Unix(0, observedAt).UTC()
	return &snapshot, nil
}

// DeleteCopilotUsageSnapshot drops a session's snapshot and cursor. A later
// sweep recreates it from scratch.
func DeleteCopilotUsageSnapshot(sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return nil
	}
	d, err := Open()
	if err != nil {
		return err
	}
	if _, err := d.Exec(`DELETE FROM copilot_usage_snapshots WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("delete copilot usage snapshot: %w", err)
	}
	return nil
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableFloat64(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}
