package sandboxpolicy_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

func TestMaterializedSandboxPinsPackPolarityAndCanonicalPaths(t *testing.T) {
	r := newRevisions()
	child := r.add(t, "child", model.SandboxPolicy{Network: &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny, Packs: []string{"net-anthropic"}}})
	root := r.add(t, "root", model.SandboxPolicy{Includes: []model.SandboxProfileRef{child}, Filesystem: []model.SandboxFilesystemRule{{HostPath: "/alias", Access: model.SandboxFilesystemRead}}, Network: &model.SandboxNetwork{Baseline: model.SandboxNetworkAllow, DenyPacks: []string{"net-github"}}})
	got, err := sandboxpolicy.MaterializeIncludes(context.Background(), root, r, canonicalPaths{"/alias": "/first"})
	require.NoError(t, err)
	require.Len(t, got.ContentHash, 64)
	require.Len(t, got.Composition.NetworkAll, 2)
	require.Empty(t, got.Composition.NetworkAll[0].Policy.Packs)
	require.Equal(t, "api.anthropic.com", got.Composition.NetworkAll[0].Policy.Allow[0].Domain)
	require.Equal(t, []uint16{443}, got.Composition.NetworkAll[0].Policy.Allow[0].Ports)
	require.Equal(t, "github.com", got.Composition.NetworkAll[1].Policy.Deny[0].Domain)
	require.Equal(t, "/first", got.Composition.Values.Filesystem[0].HostPath)
	require.Equal(t, []string{"net-anthropic"}, r.policies[child.RevisionID].Network.Packs)
	same, err := sandboxpolicy.MaterializeIncludes(context.Background(), root, r, canonicalPaths{"/alias": "/first"})
	require.NoError(t, err)
	require.Equal(t, got.ContentHash, same.ContentHash)
	changed, err := sandboxpolicy.MaterializeIncludes(context.Background(), root, r, canonicalPaths{"/alias": "/second"})
	require.NoError(t, err)
	require.NotEqual(t, got.ContentHash, changed.ContentHash)
	got.Packs[0].Entries[0].Ports[0] = 1
	got.Composition.NetworkAll[0].Policy.Allow[0].Ports[0] = 2
	require.Equal(t, uint16(443), sandboxpolicy.NetworkPackCatalog()[1].Entries[0].Ports[0])
	require.Equal(t, uint16(443), same.Composition.NetworkAll[0].Policy.Allow[0].Ports[0])
}

func TestMaterializedSandboxScopesKeepExactPrecedence(t *testing.T) {
	r := newRevisions()
	a := r.add(t, "a", model.SandboxPolicy{Environment: model.Environment{"VALUE": "a"}})
	b := r.add(t, "b", model.SandboxPolicy{Environment: model.Environment{"VALUE": "b"}})
	first, err := sandboxpolicy.MaterializeScopes(context.Background(), []sandboxpolicy.ScopeSelection{{Scope: sandboxpolicy.ScopeExplicit, Ref: b}, {Scope: sandboxpolicy.ScopeGlobal, Ref: a}}, r, nil)
	require.NoError(t, err)
	second, err := sandboxpolicy.MaterializeScopes(context.Background(), []sandboxpolicy.ScopeSelection{{Scope: sandboxpolicy.ScopeGlobal, Ref: a}, {Scope: sandboxpolicy.ScopeExplicit, Ref: b}}, r, nil)
	require.NoError(t, err)
	require.Equal(t, first.ContentHash, second.ContentHash)
	require.Equal(t, "b", first.Composition.Values.Environment["VALUE"])
	changed, err := sandboxpolicy.MaterializeScopes(context.Background(), []sandboxpolicy.ScopeSelection{{Scope: sandboxpolicy.ScopeGlobal, Ref: b}, {Scope: sandboxpolicy.ScopeExplicit, Ref: a}}, r, nil)
	require.NoError(t, err)
	require.NotEqual(t, first.ContentHash, changed.ContentHash)
}

func TestSandboxLaunchSelectionRejectsChangedResolvedContent(t *testing.T) {
	r := newRevisions()
	ref := r.add(t, "launch", model.SandboxPolicy{Environment: model.Environment{"LITERAL": "retained"}})
	materialized, err := sandboxpolicy.MaterializeScopes(context.Background(), []sandboxpolicy.ScopeSelection{{Scope: sandboxpolicy.ScopeExplicit, Ref: ref}}, r, nil)
	require.NoError(t, err)
	selected, err := materialized.LaunchSelection()
	require.NoError(t, err)
	require.Equal(t, materialized.ContentHash, selected.PolicyHash)
	require.Equal(t, ref, selected.Scopes[0].Ref)
	selected.Scopes[0].Ref.RevisionID = "changed"
	require.Equal(t, ref, materialized.Scopes[0].Ref, "public projection must not alias retained inputs")
	materialized.Composition.Values.Environment["LITERAL"] = "changed"
	_, err = materialized.LaunchSelection()
	require.ErrorContains(t, err, "content changed")
	preview, err := sandboxpolicy.MaterializeIncludes(context.Background(), ref, r, nil)
	require.NoError(t, err)
	_, err = preview.LaunchSelection()
	require.Error(t, err, "launch must resolve explicit scope instead of reinterpreting preview identity")
}
