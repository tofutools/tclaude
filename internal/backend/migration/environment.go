package migration

import (
	"encoding/json"
	"fmt"

	"github.com/tofutools/tclaude/internal/backend/app"
	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// Environment was authored as rows in v228. Retain unsupported or malformed
// sets as source evidence instead of partially applying an ambiguous override.
func (t *translator) launchEnvironment(batch *app.ImportBatch, table string, row sourcev228.Row) model.Environment {
	raw := row.Values["environment_json"]
	if config := sourcev228.String(row.Values["initial_spawn_config"]); config != "" {
		var values map[string]json.RawMessage
		if json.Unmarshal([]byte(config), &values) == nil {
			if value, ok := values["environment"]; ok {
				raw = string(value)
			}
		}
	}
	if raw == nil || sourcev228.String(raw) == "" {
		return nil
	}
	data := []byte(sourcev228.String(raw))
	var entries []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	err := json.Unmarshal(data, &entries)
	var out model.Environment
	if err == nil {
		for _, entry := range entries {
			if out == nil {
				out = model.Environment{}
			}
			if previous, exists := out[entry.Name]; exists && previous != entry.Value {
				err = fmt.Errorf("conflicting duplicate environment name")
				break
			}
			out[entry.Name] = entry.Value
		}
	}
	if err == nil {
		err = out.Validate()
	}
	if err != nil {
		t.launchMetadataDiagnostic(batch, table, row.Key, "launch_environment_retained_unmapped", "environment is retained in source evidence; its shape or runtime control names require operator review before use")
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
