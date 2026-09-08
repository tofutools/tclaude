package model

import "time"

type SandboxProfileID string
type SandboxProfileRevisionID string

func (id SandboxProfileID) Validate() error {
	return ValidateStableID("sandbox profile id", string(id))
}
func (id SandboxProfileRevisionID) Validate() error {
	return ValidateStableID("sandbox profile revision id", string(id))
}

// SandboxProfileRef identifies a profile by stable ID. Optional revision fields
// describe recorded content; authored choices need only the profile ID.
type SandboxProfileRef struct {
	ProfileID   SandboxProfileID
	RevisionID  SandboxProfileRevisionID
	ContentHash string
}

type SandboxProfile struct {
	ID             SandboxProfileID
	Name           string
	HeadRevisionID SandboxProfileRevisionID
	Archived       bool
	// Imported profiles retain source identity and can only be reused by independent copy.
	Imported  bool `json:",omitempty"`
	Revision  Revision
	CreatedAt time.Time
	UpdatedAt time.Time
}

type SandboxProfileRevision struct {
	Ref       SandboxProfileRef
	Number    Revision
	Policy    SandboxPolicy
	Author    Principal
	RequestID RequestID
	CreatedAt time.Time
}

// SandboxPolicy is the authored host-isolation policy. Native approval and
// native SandboxMode remain separate desired settings. No field is evidence
// that the host has materialized or enforced a policy.
type SandboxPolicy struct {
	Includes                []SandboxProfileRef     `json:",omitempty"`
	Filesystem              []SandboxFilesystemRule `json:",omitempty"`
	Tmpfs                   []SandboxTmpfs          `json:",omitempty"`
	Environment             Environment             `json:",omitempty"`
	AgentDirectories        []string                `json:",omitempty"`
	FilesystemRoot          SandboxFilesystemRoot   `json:",omitempty"`
	HarnessConfig           SandboxHarnessConfig    `json:",omitempty"`
	Network                 *SandboxNetwork         `json:",omitempty"`
	UnixSockets             *SandboxUnixSockets     `json:",omitempty"`
	Resources               SandboxResources        `json:",omitempty"`
	DarwinAllowMachRegister bool                    `json:",omitempty"`
	PreLaunch               []SandboxSetupBlock     `json:",omitempty"`
}

type SandboxFilesystemAccess string

const (
	SandboxFilesystemRead  SandboxFilesystemAccess = "read"
	SandboxFilesystemWrite SandboxFilesystemAccess = "write"
	SandboxFilesystemDeny  SandboxFilesystemAccess = "deny"
)

// HostPath is authority-bearing. GuestPath names only the child namespace.
// ExpectedKind retains a file commitment so replacement by a directory cannot
// widen an admitted grant. Host resolution supplies canonical identity separately.
type SandboxFilesystemRule struct {
	HostPath     string
	Access       SandboxFilesystemAccess
	GuestPath    string `json:",omitempty"`
	ExpectedKind string `json:",omitempty"`
}

type SandboxTmpfs struct {
	GuestPath string
	Size      string `json:",omitempty"`
}

type SandboxFilesystemRoot string

const (
	SandboxRootAutomatic SandboxFilesystemRoot = ""
	SandboxRootInherit   SandboxFilesystemRoot = "inherit"
	SandboxRootSeparate  SandboxFilesystemRoot = "separate"
)

type SandboxHarnessConfig string

const (
	SandboxHarnessConfigDefault SandboxHarnessConfig = ""
	SandboxHarnessConfigRead    SandboxHarnessConfig = "read"
	SandboxHarnessConfigWrite   SandboxHarnessConfig = "write"
)

type SandboxNetworkBaseline string

const (
	SandboxNetworkInherit SandboxNetworkBaseline = "inherit"
	SandboxNetworkAllow   SandboxNetworkBaseline = "allow"
	SandboxNetworkDeny    SandboxNetworkBaseline = "deny"
)

type SandboxNetworkEngine string

const (
	SandboxNetworkPacket SandboxNetworkEngine = "packet"
	SandboxNetworkProxy  SandboxNetworkEngine = "proxy"
)

type SandboxNetwork struct {
	Baseline  SandboxNetworkBaseline
	Allow     []SandboxDestination `json:",omitempty"`
	Deny      []SandboxDestination `json:",omitempty"`
	Packs     []string             `json:",omitempty"`
	DenyPacks []string             `json:",omitempty"`
	Namespace string               `json:",omitempty"`
	Engine    SandboxNetworkEngine `json:",omitempty"`
}

type SandboxDestination struct {
	Host              string   `json:",omitempty"`
	Domain            string   `json:",omitempty"`
	IncludeSubdomains bool     `json:",omitempty"`
	CIDR              string   `json:",omitempty"`
	Loopback          bool     `json:",omitempty"`
	Ports             []uint16 `json:",omitempty"`
}

type SandboxUnixSockets struct {
	Mode  string
	Allow []SandboxSocketSelector `json:",omitempty"`
}
type SandboxSocketSelector struct {
	Path     string `json:",omitempty"`
	PathGlob string `json:",omitempty"`
}

// Quantities retain authored spelling. Host planning derives numeric limits;
// callers cannot submit a second, contradictory numeric authority value.
type SandboxResources struct {
	Memory string `json:",omitempty"`
	CPU    string `json:",omitempty"`
}

// Setup is executable authoring material, not an automatic save-time effect.
// Blocks run in authored order only inside the admitted host boundary.
type SandboxSetupBlock struct {
	Name    string
	Script  string
	Exports []string `json:",omitempty"`
}
