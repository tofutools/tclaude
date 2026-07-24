package db

import (
	"encoding/json"
	"log/slog"
)

// JSON-object TEXT columns that are not permission overrides.
//
// marshalPermissionOverrides / unmarshalPermissionOverrides already encode a
// map[string]string the same way, but their warning text names the pending-spawn
// permission path — so reusing them for, say, a corrupt
// sessions.context_features blob logs "failed to unmarshal permission
// overrides", pointing a future debugger at the wrong feature entirely. These
// carry the column name instead.
//
// Same degradation contract as the permission helpers: a malformed blob logs and
// yields nil rather than failing the read, because a corrupt cosmetic column must
// not wedge a session load.

func marshalStringMapColumn(m map[string]string, column string) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		slog.Warn("failed to marshal JSON map column; storing empty", "column", column, "error", err)
		return ""
	}
	return string(b)
}

func unmarshalStringMapColumn(s, column string) map[string]string {
	if s == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		slog.Warn("failed to unmarshal JSON map column; treating as empty", "column", column, "error", err)
		return nil
	}
	return m
}
