package model

import "time"

// GroupConfiguration selects a saved profile for explicit new-member creation.
// Its stored revision metadata does not freeze future profile edits.
// It never changes existing members or dynamically inherits from parent groups.
type GroupConfiguration struct {
	Environment Environment `json:",omitempty"`
	GroupID     GroupID
	Profile     *ConfigurationProfileRef
	Revision    Revision
	UpdatedAt   time.Time
}
