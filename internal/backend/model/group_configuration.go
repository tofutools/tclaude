package model

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// GroupConfiguration selects a saved profile for explicit new-member creation.
// Its stored revision metadata does not freeze future profile edits.
// It never changes existing members or dynamically inherits from parent groups.
type GroupConfiguration struct {
	DefaultDirectory string      `json:",omitempty"`
	Environment      Environment `json:",omitempty"`
	GroupID          GroupID
	Profile          *ConfigurationProfileRef
	Revision         Revision
	UpdatedAt        time.Time
}

// ValidateDefaultDirectory validates stored intent without requiring the directory
// to exist yet. Host shorthand expansion belongs to the application host port.
func ValidateDefaultDirectory(path string) error {
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 4096 || !utf8.ValidString(path) || strings.ContainsRune(path, 0) {
		return fmt.Errorf("default directory requires an absolute clean path")
	}
	return nil
}
