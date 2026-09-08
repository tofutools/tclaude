package model_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestSandboxSelectionRetainsExactPolicyAndScopeIdentity(t *testing.T) {
	ref := model.SandboxProfileRef{ProfileID: "profile", RevisionID: "revision", ContentHash: strings.Repeat("a", 64)}
	selected := model.SandboxSelection{Scopes: []model.SandboxScopeSelection{{Scope: model.SandboxScopeGlobal, Ref: ref}, {Scope: model.SandboxScopeExplicit, Ref: ref}}, PolicyHash: strings.Repeat("b", 64)}
	require.NoError(t, selected.Validate())
	copied := selected.Clone()
	copied.Scopes[1].Ref.RevisionID = "next"
	require.Equal(t, model.SandboxProfileRevisionID("revision"), selected.Scopes[1].Ref.RevisionID)
	require.False(t, selected.Equal(copied))
	copied = selected.Clone()
	copied.PolicyHash = strings.Repeat("c", 64)
	require.False(t, selected.Equal(copied), "resolved content changes even when profile labels do not")
	for _, change := range []func(*model.SandboxSelection){
		func(s *model.SandboxSelection) { s.Scopes = nil },
		func(s *model.SandboxSelection) { s.Scopes[1].Scope = model.SandboxScopeGlobal },
		func(s *model.SandboxSelection) { s.Scopes[0].Scope = "unknown" },
		func(s *model.SandboxSelection) { s.Scopes[0], s.Scopes[1] = s.Scopes[1], s.Scopes[0] },
		func(s *model.SandboxSelection) { s.Scopes[0].Ref.RevisionID = "bad id" },
		func(s *model.SandboxSelection) { s.Scopes[0].Ref.ContentHash = "" },
		func(s *model.SandboxSelection) { s.PolicyHash = strings.Repeat("B", 64) },
	} {
		invalid := selected.Clone()
		change(&invalid)
		require.Error(t, invalid.Validate())
	}
}
