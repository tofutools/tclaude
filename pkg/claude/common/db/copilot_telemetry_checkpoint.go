package db

import (
	"encoding/json"
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
// checkpoint. Validation belongs to the harness package.
func SaveCopilotTelemetryCheckpoint(sessionID string, data json.RawMessage) error {
	return SaveCodexTelemetryCheckpoint(sessionID, data)
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
