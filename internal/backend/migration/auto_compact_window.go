package migration

import (
	"encoding/json"
	"fmt"
	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func importedAutoCompactWindow(value any) (model.AutoCompactWindow, error) {
	if value == nil {
		return "", nil
	}
	raw, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("auto-compaction window must be text")
	}
	normalized, err := model.ParseAutoCompactWindow(raw)
	return model.AutoCompactWindow(normalized), err
}
func validateImportedAutoCompactWindow(values map[string]any, harness string) error {
	if raw := sourcev228.String(values["initial_spawn_config"]); raw != "" {
		var config map[string]any
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			return err
		}
		values = config
	}
	window, err := importedAutoCompactWindow(values["auto_compact_window"])
	if err != nil {
		return err
	}
	return window.Validate(harness)
}
