package migration

import (
	"encoding/json"
	"fmt"
	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func importedAutoMemory(value any) (bool, error) {
	if value == nil {
		return false, nil
	}
	switch fmt.Sprint(value) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	}
	return false, fmt.Errorf("invalid auto-memory value")
}
func validateImportedAutoMemory(values map[string]any, harness string) error {
	if raw := sourcev228.String(values["initial_spawn_config"]); raw != "" {
		var config map[string]any
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			return err
		}
		if value := config["auto_memory"]; value != nil {
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("birth auto-memory must be boolean")
			}
		}
		enabled, err := importedAutoMemory(config["auto_memory"])
		if err != nil {
			return err
		}
		return model.ValidateAutoMemory(enabled, harness)
	}
	enabled, err := importedAutoMemory(values["auto_memory"])
	if err != nil {
		return err
	}
	return model.ValidateAutoMemory(enabled, harness)
}
