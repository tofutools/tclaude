package db

import (
	"context"
	"encoding/json"
	"time"
)

// Copilot's durable follower checkpoint.
//
// It shares the `codex_telemetry_checkpoints` TABLE with Codex, deliberately.
// The table stores one opaque blob per session id, keyed by nothing else, and
// its own documentation already calls that blob an "opaque harness follower
// checkpoint" — the DB layer never decodes it and cannot, since parsing it
// would need the harness package and create an import cycle. A session id
// belongs to exactly one harness, so two harnesses' checkpoints can share the
// keyspace without any chance of one reading the other's row.
//
// Adding a second, structurally identical table would therefore buy a better
// name and nothing else, at the cost of a migration and a second set of
// pruning/cleanup call sites to keep in step. The `codex_` prefix is the
// historical naming CLAUDE.md describes: accurate about where the table came
// from, not a claim about what may live in it.
//
// These wrappers exist so Copilot's call sites read as Copilot's, and so a
// later rename of the table touches one file per harness rather than every
// caller.

// SaveCopilotTelemetryCheckpoint replaces one Copilot session's follower
// checkpoint, but only while the session row is still the generation the
// caller observed. Validation of the blob belongs to the harness package.
//
// The generation guard matters because a session id can be pruned and
// recreated while the daemon's in-memory follower survives. The existence
// check and the UPSERT are one statement, so the recreated row cannot end up
// holding the previous conversation's cursor and totals.
//
// Reports whether the write landed; false means the generation had already
// moved on, which is a normal outcome and not an error.
func SaveCopilotTelemetryCheckpoint(
	sessionID, convID string,
	createdAt time.Time,
	data json.RawMessage,
) (bool, error) {
	return SaveCodexTelemetryCheckpointForSessionGenerationContext(
		context.Background(), sessionID, convID, createdAt, data)
}

// LoadCopilotTelemetryCheckpoint returns one Copilot session's checkpoint, or
// nil when none exists.
func LoadCopilotTelemetryCheckpoint(sessionID string) (*CodexTelemetryCheckpointRow, error) {
	return LoadCodexTelemetryCheckpoint(sessionID)
}

// DeleteCopilotTelemetryCheckpoint drops a checkpoint the follower refused.
// A subsequent successful full scan recreates it.
func DeleteCopilotTelemetryCheckpoint(sessionID string) error {
	return DeleteCodexTelemetryCheckpoint(sessionID)
}
