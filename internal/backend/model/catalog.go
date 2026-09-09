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
	Aliases           []string `json:",omitempty"`
	Disabled          bool     `json:",omitempty"`
	DisabledReason    string   `json:",omitempty"`
	Archived          bool     `json:",omitempty"`
	ID                ConfigurationProfileID
	Name              string
	CurrentRevisionID ConfigurationProfileRevisionID
	Revision          Revision
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ProfileStartup contains reusable operator-reviewed launch suggestions, not native settings.
type ProfileStartup struct {
	Role           string `json:",omitempty"`
	Description    string `json:",omitempty"`
	AgentName      string
	Context        string
	InitialMessage string
}

type ConfigurationProfileRevision struct {
	Startup   *ProfileStartup `json:",omitempty"`
	Ref       ConfigurationProfileRef
	Desired   DesiredConfiguration
	CreatedAt time.Time
}

// ConfigurationDefaults selects profiles by identity for future agents. Revision
// metadata records the selection; new creation resolves the current profile.
type ConfigurationDefaults struct {
	Global    *ConfigurationProfileRef
	Harnesses map[string]ConfigurationProfileRef
	Revision  Revision
	UpdatedAt time.Time
}
