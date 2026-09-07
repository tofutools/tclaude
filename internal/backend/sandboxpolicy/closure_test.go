package sandboxpolicy_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

type revisions struct {
	policies map[model.SandboxProfileRevisionID]model.SandboxPolicy
	reads    map[model.SandboxProfileRevisionID]int
}

func (r *revisions) ReadSandboxRevision(_ context.Context, ref model.SandboxProfileRef) (model.SandboxPolicy, error) {
	r.reads[ref.RevisionID]++
	p, ok := r.policies[ref.RevisionID]
	if !ok {
		return model.SandboxPolicy{}, fmt.Errorf("missing revision")
	}
	return p, nil
}
func (r *revisions) add(t *testing.T, id string, p model.SandboxPolicy) model.SandboxProfileRef {
	t.Helper()
	hash, err := sandboxpolicy.ContentHash(p)
	require.NoError(t, err)
	ref := model.SandboxProfileRef{ProfileID: model.SandboxProfileID("sandbox_" + id), RevisionID: model.SandboxProfileRevisionID("revision_" + id), ContentHash: hash}
	r.policies[ref.RevisionID] = p
	return ref
}
func newRevisions() *revisions {
	return &revisions{policies: map[model.SandboxProfileRevisionID]model.SandboxPolicy{}, reads: map[model.SandboxProfileRevisionID]int{}}
}

func TestSandboxClosurePinsSharedGraphWithoutFlatteningOrAliasing(t *testing.T) {
	r := newRevisions()
	shared := r.add(t, "shared", model.SandboxPolicy{Environment: model.Environment{"VALUE": "shared"}, PreLaunch: []model.SandboxSetupBlock{{Name: "setup", Script: "echo shared", Exports: []string{"VALUE"}}}})
	first := r.add(t, "first", model.SandboxPolicy{Includes: []model.SandboxProfileRef{shared}, Environment: model.Environment{"VALUE": "first"}})
	second := r.add(t, "second", model.SandboxPolicy{Includes: []model.SandboxProfileRef{shared}, Environment: model.Environment{"VALUE": "second"}})
	root := r.add(t, "root", model.SandboxPolicy{Includes: []model.SandboxProfileRef{first, second}})
	closure, err := sandboxpolicy.Resolve(context.Background(), root, r)
	require.NoError(t, err)
	require.Equal(t, []model.SandboxProfileRef{first, second}, closure.Entries[3].Policy.Includes)
	require.Len(t, closure.Entries, 4)
	require.Equal(t, 1, r.reads[shared.RevisionID])
	require.Equal(t, shared, closure.Entries[0].Ref)
	r.policies[shared.RevisionID].Environment["VALUE"] = "later mutation"
	r.policies[shared.RevisionID].PreLaunch[0].Exports[0] = "OTHER"
	require.Equal(t, "shared", closure.Entries[0].Policy.Environment["VALUE"])
	require.Equal(t, []string{"VALUE"}, closure.Entries[0].Policy.PreLaunch[0].Exports)
	_, err = sandboxpolicy.Resolve(context.Background(), root, r)
	require.ErrorContains(t, err, "pinned hash")
}

func TestSandboxClosureBoundsCachedSubgraphDepth(t *testing.T) {
	r := newRevisions()
	leaf := r.add(t, "leaf", model.SandboxPolicy{})
	previous := leaf
	for i := 0; i < 2; i++ {
		previous = r.add(t, fmt.Sprintf("shared%d", i), model.SandboxPolicy{Includes: []model.SandboxProfileRef{previous}})
	}
	shared := previous
	// The first root edge caches the shared height of two. Its second edge
	// reaches that same graph through 14 more edges: 1 + 14 + 2 exceeds 16.
	for i := 0; i < 14; i++ {
		previous = r.add(t, fmt.Sprintf("deep%d", i), model.SandboxPolicy{Includes: []model.SandboxProfileRef{previous}})
	}
	root := r.add(t, "root", model.SandboxPolicy{Includes: []model.SandboxProfileRef{shared, previous}})
	_, err := sandboxpolicy.Resolve(context.Background(), root, r)
	require.ErrorContains(t, err, "16 edges")
}

func TestSandboxClosureRejectsMissingAndConflictingReferences(t *testing.T) {
	r := newRevisions()
	first := r.add(t, "same", model.SandboxPolicy{Environment: model.Environment{"VALUE": "first"}})
	second := first
	hash, err := sandboxpolicy.ContentHash(model.SandboxPolicy{Environment: model.Environment{"VALUE": "second"}})
	require.NoError(t, err)
	second.ContentHash = hash
	branch := r.add(t, "branch", model.SandboxPolicy{Includes: []model.SandboxProfileRef{second}})
	root := r.add(t, "root", model.SandboxPolicy{Includes: []model.SandboxProfileRef{first, branch}})
	_, err = sandboxpolicy.Resolve(context.Background(), root, r)
	require.ErrorContains(t, err, "conflicting exact references")
	delete(r.policies, first.RevisionID)
	_, err = sandboxpolicy.Resolve(context.Background(), first, r)
	require.ErrorContains(t, err, "missing revision")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = sandboxpolicy.Resolve(ctx, root, r)
	require.ErrorIs(t, err, context.Canceled)
}

func TestSandboxContentIdentityTreatsEmptyEnvironmentAsAbsent(t *testing.T) {
	absent, err := sandboxpolicy.ContentHash(model.SandboxPolicy{})
	require.NoError(t, err)
	empty, err := sandboxpolicy.ContentHash(model.SandboxPolicy{Environment: model.Environment{}})
	require.NoError(t, err)
	require.Equal(t, absent, empty)
}
