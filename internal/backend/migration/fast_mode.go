package migration

import (
	"fmt"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// SQLite profiles use nullable 0/1; birth and resolved records use nullable booleans.
func importedFastMode(value any) model.FastMode {
	if value == nil {
		return ""
	}
	switch fmt.Sprint(value) {
	case "true", "1":
		return model.FastModeOn
	case "false", "0":
		return model.FastModeOff
	}
	return model.FastMode(fmt.Sprint(value))
}
