// Package v228 is the only legacy-schema-aware part of the replacement
// importer. It never discovers or migrates a source database.
package v228

import "encoding/json"

const SchemaVersion = 228

type Row struct {
	Key    string
	Values map[string]any
}

// Snapshot holds original rows needed by a later target writer. It must never
// be serialized as a user-facing report because it can contain message bodies,
// authored configuration, and path metadata.
type Snapshot struct {
	Rows      map[string][]Row
	Config    json.RawMessage
	Malformed map[string]int64
}
