package federationcmd

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"testing"
)

func TestNodesCLI(t *testing.T) {
	require.NotNil(t, nodesCmd().Flags().Lookup("match"))
	require.NotNil(t, nodesCmd().Flags().Lookup("json"))
	require.NotNil(t, nodeLabelsCmd().Flags().Lookup("add"))
	var b bytes.Buffer
	printNodes(&b, []remoteNode{{Peer: "mac", Stale: true, NodeMetadata: &proto.NodeMetadata{OS: "darwin", Arch: "arm64", Labels: []string{"gpu"}, Resources: proto.NodeResources{Status: "stale", Agents: &proto.NodeAgents{LiveAgents: 2}, RAM: &proto.NodeRAM{AvailableBytes: 1 << 30, AvailableEstimated: true}}}}})
	for _, s := range []string{"mac", "darwin", "gpu", "offline", "1.0 GiB (est.)", "2 / unlimited", "unavailable"} {
		require.Contains(t, b.String(), s)
	}
}
