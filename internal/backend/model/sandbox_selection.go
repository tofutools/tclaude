package model

import (
	"encoding/hex"
	"fmt"
	"slices"
)

// SandboxProfileScope describes where a host profile was selected.
// Its order is fixed and is independent of native SandboxMode.
type SandboxProfileScope string

const (
	SandboxScopeGlobal   SandboxProfileScope = "global"
	SandboxScopeGroup    SandboxProfileScope = "group"
	SandboxScopeExplicit SandboxProfileScope = "explicit"
)

type SandboxScopeSelection struct {
	Scope SandboxProfileScope
	Ref   SandboxProfileRef
}

// SandboxSelection stores profile choices by stable ID. Revision and policy
// hashes are optional preparation evidence, never a requirement for choosing a
// profile or a request to keep using old content on a later launch.
type SandboxSelection struct {
	GroupID      GroupID `json:",omitempty"`
	OmitProfiles bool    `json:",omitempty"`
	Scopes       []SandboxScopeSelection
	PolicyHash   string `json:",omitempty"`
}

func (s SandboxSelection) Validate() error {
	if s.GroupID != "" && s.GroupID.Validate() != nil {
		return fmt.Errorf("invalid sandbox group")
	}
	if s.OmitProfiles {
		if len(s.Scopes) != 0 || s.GroupID != "" {
			return fmt.Errorf("omitted profiles cannot select scopes")
		}
		return nil
	}
	if len(s.Scopes) == 0 && s.PolicyHash == "" && s.GroupID != "" {
		return nil
	}
	if len(s.Scopes) == 0 || len(s.Scopes) > 3 || s.PolicyHash != "" && !sandboxDigest(s.PolicyHash) {
		return fmt.Errorf("host sandbox selection requires ordered profile scopes")
	}
	previous := -1
	for _, selected := range s.Scopes {
		rank := slices.Index([]SandboxProfileScope{SandboxScopeGlobal, SandboxScopeGroup, SandboxScopeExplicit}, selected.Scope)
		if rank <= previous || selected.Ref.ProfileID.Validate() != nil || (selected.Ref.RevisionID != "" && selected.Ref.RevisionID.Validate() != nil) || (selected.Ref.ContentHash != "" && !sandboxDigest(selected.Ref.ContentHash)) {
			return fmt.Errorf("host sandbox scopes must be unique ordered profile references")
		}
		previous = rank
	}
	return nil
}

func (s SandboxSelection) Clone() SandboxSelection {
	s.Scopes = slices.Clone(s.Scopes)
	return s
}

func (s SandboxSelection) Equal(other SandboxSelection) bool {
	return s.GroupID == other.GroupID && s.OmitProfiles == other.OmitProfiles && s.PolicyHash == other.PolicyHash && slices.Equal(s.Scopes, other.Scopes)
}

func sandboxDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

// CloneSandboxSelection preserves absence and detaches retained scope inputs.
func CloneSandboxSelection(s *SandboxSelection) *SandboxSelection {
	if s == nil {
		return nil
	}
	copied := s.Clone()
	return &copied
}

func SameSandboxSelection(a, b *SandboxSelection) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func ValidateSandboxSelection(s *SandboxSelection) error {
	if s == nil {
		return nil
	}
	return s.Validate()
}

// ValidateHostSandboxProfiles validates delegated profile IDs, not content hashes.
func (b ConfigurationBounds) ValidateHostSandboxProfiles() error {
	if len(b.HostSandboxProfiles) > 128 {
		return fmt.Errorf("too many host sandbox profile IDs")
	}
	seen := map[string]bool{}
	for _, id := range b.HostSandboxProfiles {
		if SandboxProfileID(id).Validate() != nil || seen[id] {
			return fmt.Errorf("host sandbox profile IDs must be unique stable IDs")
		}
		seen[id] = true
	}
	return nil
}

// References removes preparation evidence from an authored selection.
func (s SandboxSelection) References() SandboxSelection {
	s = s.Clone()
	s.PolicyHash = ""
	for i := range s.Scopes {
		s.Scopes[i].Ref = SandboxProfileRef{ProfileID: s.Scopes[i].Ref.ProfileID}
	}
	return s
}

func SameSandboxProfiles(a, b *SandboxSelection) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.References().Equal(b.References())
}
