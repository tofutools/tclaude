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
	for id, scoped := range labels.Groups {
		if err := id.Validate(); err != nil {
			return err
		}
		if err := (AgentLabels{Role: scoped.Role, Description: scoped.Description}).Validate(); err != nil {
			return err
		}
	}
	return nil
}

// InGroup preserves group-specific display metadata, including an explicit empty label.
func (labels AgentLabels) InGroup(id GroupID) AgentDisplayLabels {
	if scoped, ok := labels.Groups[id]; ok {
		return scoped
	}
	return AgentDisplayLabels{Role: labels.Role, Description: labels.Description}
}

// ValidateRoleLabelTarget keeps display-role matching within an explicit group.
func (a MessageAudience) ValidateRoleLabelTarget() error {
	if a.RoleLabel == "" {
		return nil
	}
	if a.GroupID.Validate() != nil || a.RoleID != "" || len(a.AgentIDs) > 0 || a.Operator || strings.TrimSpace(a.RoleLabel) == "" {
		return fmt.Errorf("a display-role target requires only an explicit group and a nonempty display role")
	}
	return (AgentLabels{Role: a.RoleLabel}).Validate()
}
