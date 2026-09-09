package migration

import (
	"encoding/json"
	"fmt"
	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func importedAskUserQuestionTimeout(value any) (model.AskUserQuestionTimeout, error) {
	if value == nil {
		return "", nil
	}
	raw, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("question timeout must be text")
	}
	normalized, err := model.ParseAskUserQuestionTimeout(raw)
	return model.AskUserQuestionTimeout(normalized), err
}
func validateImportedAskUserQuestionTimeout(values map[string]any, harness string) error {
	if raw := sourcev228.String(values["initial_spawn_config"]); raw != "" {
		var config map[string]any
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			return err
		}
		values = config
	}
	window, err := importedAskUserQuestionTimeout(values["ask_user_question_timeout"])
	if err != nil {
		return err
	}
	return window.Validate(harness)
}
