package sandboxpolicy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestSandboxBundleRefusesMissingTamperedAndUnreachableContent(t *testing.T) {
	ctx := context.Background()
	policy := model.SandboxPolicy{Environment: model.Environment{"VALUE": "literal"}}
	hash, err := ContentHash(policy)
	require.NoError(t, err)
	ref := model.SandboxProfileRef{ProfileID: "profile", RevisionID: "revision", ContentHash: hash}
	valid := Bundle{Format: BundleFormat, Version: 1, Root: ref, Entries: []BundleEntry{{Ref: ref, Name: "Profile", Policy: policy}}}
	copy, err := InspectBundle(ctx, valid)
	require.NoError(t, err)
	copy.Entries[0].Policy.Environment["VALUE"] = "changed"
	require.Equal(t, "literal", policy.Environment["VALUE"])
	_, err = InspectBundle(ctx, copy)
	require.ErrorIs(t, err, ErrInvalidClosure)
	bad := valid
	bad.Version = 2
	_, err = InspectBundle(ctx, bad)
	require.ErrorIs(t, err, ErrInvalidClosure)
	bad = valid
	bad.Root.RevisionID = "missing"
	_, err = InspectBundle(ctx, bad)
	require.ErrorIs(t, err, ErrInvalidClosure)
	bad = valid
	extra := valid.Entries[0]
	extra.Ref.RevisionID = "unreachable"
	bad.Entries = append([]BundleEntry{extra}, valid.Entries...)
	_, err = InspectBundle(ctx, bad)
	require.ErrorIs(t, err, ErrInvalidClosure)
	bad = valid
	bad.Entries = append([]BundleEntry{}, valid.Entries...)
	bad.Entries[0].Name = string([]byte{255})
	_, err = InspectBundle(ctx, bad)
	require.ErrorIs(t, err, ErrInvalidClosure)
}
