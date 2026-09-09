package migration

import (
	"encoding/json"
	"fmt"
	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func importedAutoReview(value any) (bool, error) {
	if value == nil {
		return false, nil
	}
	switch fmt.Sprint(value) {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	}
	return false, fmt.Errorf("invalid automatic approval review value")
}
func validateImportedAutoReview(values map[string]any, harness string) error {
	if raw := sourcev228.String(values["initial_spawn_config"]); raw != "" {
		var config map[string]any
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			return err
		}
		if value := config["auto_review"]; value != nil {
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("birth automatic approval review must be boolean")
			}
		}
		enabled, err := importedAutoReview(config["auto_review"])
		if err != nil {
			return err
		}
		return model.ValidateAutoReview(enabled, harness)
	}
	enabled, err := importedAutoReview(values["auto_review"])
	if err != nil {
		return err
	}
	return model.ValidateAutoReview(enabled, harness)
}
