package model

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Validate checks display metadata only. Labels never grant role authority.
func (labels AgentLabels) Validate() error {
	for _, value := range []string{labels.Role, labels.Description} {
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("agent labels require valid text without NUL")
		}
	}
	return nil
}
