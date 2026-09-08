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
