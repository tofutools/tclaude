package agentd

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"testing"
)

func TestCatalogStructureRetriesOnlyFailedPeer(t *testing.T) {
	rt := &fedRuntime{online: map[string]bool{"a": true, "b": true}}
	peers := []db.FederationPeer{{InstanceID: "a"}, {InstanceID: "b"}}
	calls := map[string]int{}
	send := func(peer string) bool { calls[peer]++; return peer == "a" || calls[peer] > 1 }
	rt.publishCatalogStructure("new-group", peers, send)
	require.Equal(t, map[string]int{"a": 1, "b": 1}, calls)
	rt.publishCatalogStructure("new-group", peers, send)
	require.Equal(t, map[string]int{"a": 1, "b": 2}, calls, "unchanged structure retries only the failed peer")
	rt.publishCatalogStructure("new-group", peers, send)
	require.Equal(t, map[string]int{"a": 1, "b": 2}, calls, "delivered unchanged catalogs generate no traffic")
	rt.publishCatalogStructure("renamed-group", peers, send)
	require.Equal(t, map[string]int{"a": 2, "b": 3}, calls)
}
