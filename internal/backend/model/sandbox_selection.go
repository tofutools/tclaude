package model

import (
	"encoding/hex"
	"fmt"
	"slices"
)

// SandboxProfileScope describes where an immutable host policy was selected.
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

// SandboxSelection retains the exact authoring inputs and resolved policy
// identity selected for a launch. The hash includes canonical paths, included
// revisions and expanded network packs; it is not evidence of OS enforcement.
// Fresh selection resolves defaults before constructing this value. Retries
// retain this value instead of resolving later defaults again.
type SandboxSelection struct {
	Scopes     []SandboxScopeSelection
	PolicyHash string
}

func (s SandboxSelection) Validate() error {
	if len(s.Scopes) == 0 || len(s.Scopes) > 3 || !sandboxDigest(s.PolicyHash) {
		return fmt.Errorf("host sandbox selection requires exact scopes and policy identity")
	}
	previous := -1
	for _, selected := range s.Scopes {
		rank := slices.Index([]SandboxProfileScope{SandboxScopeGlobal, SandboxScopeGroup, SandboxScopeExplicit}, selected.Scope)
		if rank <= previous || selected.Ref.ProfileID.Validate() != nil || selected.Ref.RevisionID.Validate() != nil || !sandboxDigest(selected.Ref.ContentHash) {
			return fmt.Errorf("host sandbox scopes must be unique ordered immutable references")
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
	return s.PolicyHash == other.PolicyHash && slices.Equal(s.Scopes, other.Scopes)
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

// ValidateHostSandboxPolicies keeps delegated policy identities bounded and exact.
func (b ConfigurationBounds) ValidateHostSandboxPolicies() error {
	if len(b.HostSandboxPolicies) > 128 {
		return fmt.Errorf("too many host sandbox policy identities")
	}
	seen := map[string]bool{}
	for _, hash := range b.HostSandboxPolicies {
		if !sandboxDigest(hash) || seen[hash] {
			return fmt.Errorf("host sandbox policy identities must be unique SHA-256 values")
		}
		seen[hash] = true
	}
	return nil
}
