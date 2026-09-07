package model

import "time"

// ConfigurationProfile is a named catalog entry. Its revisions are immutable;
// changing the current revision never changes an existing Agent or Execution.
type ConfigurationProfileID string
type ConfigurationProfileRevisionID string

type ConfigurationProfileRef struct {
	ProfileID   ConfigurationProfileID
	RevisionID  ConfigurationProfileRevisionID
	ContentHash string
}

type ConfigurationProfile struct {
	Archived          bool `json:",omitempty"`
	ID                ConfigurationProfileID
	Name              string
	CurrentRevisionID ConfigurationProfileRevisionID
	Revision          Revision
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type ConfigurationProfileRevision struct {
	Ref       ConfigurationProfileRef
	Desired   DesiredConfiguration
	CreatedAt time.Time
}

// ConfigurationDefaults pins explicit saved revisions. An edited profile does
// not move these choices; selecting a new default is a separate CAS mutation.
type ConfigurationDefaults struct {
	Global    *ConfigurationProfileRef
	Harnesses map[string]ConfigurationProfileRef
	Revision  Revision
	UpdatedAt time.Time
}
