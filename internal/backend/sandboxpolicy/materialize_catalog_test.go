package sandboxpolicy

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestMaterializationIdentityChangesWithReleasePackContent(t *testing.T) {
	composed := Composition{NetworkAll: []NetworkConjunct{{Policy: model.SandboxNetwork{Baseline: model.SandboxNetworkDeny, Packs: []string{"net-anthropic"}}}}}
	catalog := NetworkPackCatalog()
	first, err := materialize(composed, nil, catalog)
	require.NoError(t, err)
	catalog[1].Label = "New display label"
	relabelled, err := materialize(composed, nil, catalog)
	require.NoError(t, err)
	require.Equal(t, first.ContentHash, relabelled.ContentHash)
	// Even an unchanged catalog ID/hash cannot hide changed executable contents.
	catalog[1].Entries[0].Domain = "different.example"
	changed, err := materialize(composed, nil, catalog)
	require.NoError(t, err)
	require.NotEqual(t, first.ContentHash, changed.ContentHash)
	require.Equal(t, "api.anthropic.com", first.Composition.NetworkAll[0].Policy.Allow[0].Domain)
	_, err = materialize(composed, nil, nil)
	require.ErrorIs(t, err, ErrInvalidClosure)
}
