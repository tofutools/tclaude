package db

import (
	"encoding/json"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func marshalEnvironmentColumn(entries []sandboxpolicy.EnvironmentEntry) (string, error) {
	normalized, err := sandboxpolicy.NormalizeEnvironment(entries)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func unmarshalEnvironmentColumn(raw string) ([]sandboxpolicy.EnvironmentEntry, error) {
	var entries []sandboxpolicy.EnvironmentEntry
	if raw == "" {
		raw = "[]"
	}
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, err
	}
	return sandboxpolicy.NormalizeEnvironment(entries)
}
