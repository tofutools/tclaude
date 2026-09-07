package sandboxpolicy_test

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
	"testing"
)

func TestNetworkPackCatalogPreservesExactDetachedDestinationSets(t *testing.T) {
	catalog := sandboxpolicy.NetworkPackCatalog()
	require.Len(t, catalog, 8)
	seen := map[string]bool{}
	for _, pack := range catalog {
		require.False(t, seen[pack.ID])
		seen[pack.ID] = true
		require.Len(t, pack.ContentHash, 64)
		require.NoError(t, sandboxpolicy.Validate(model.SandboxPolicy{Network: &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny, Allow: pack.Entries}}))
	}
	original := catalog[1].ContentHash
	require.Equal(t, "api.anthropic.com", catalog[1].Entries[0].Domain)
	require.Equal(t, []uint16{443}, catalog[1].Entries[0].Ports)
	catalog[1].Entries[0].Domain = "changed.invalid"
	catalog[1].Entries[0].Ports[0] = 80
	fresh := sandboxpolicy.NetworkPackCatalog()
	require.Equal(t, "api.anthropic.com", fresh[1].Entries[0].Domain)
	require.Equal(t, []uint16{443}, fresh[1].Entries[0].Ports)
	require.Equal(t, original, fresh[1].ContentHash)
}
