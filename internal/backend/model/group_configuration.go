package model

import "time"

// GroupConfiguration pins a saved revision for explicit new-member creation.
// It never changes existing members or dynamically inherits from parent groups.
type GroupConfiguration struct {
	GroupID   GroupID
	Profile   *ConfigurationProfileRef
	Revision  Revision
	UpdatedAt time.Time
}
