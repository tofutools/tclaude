package proto

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func TestNodeMetadataSanitizationAndMatch(t *testing.T) {
	at := time.Now()
	n := &NodeMetadata{Schema: 1, OS: "darwin", Arch: "arm64", OSVersion: "14\x1b[31m", Labels: []string{"gpu", "gpu", "test-rig", "bad\nlabel"}, Harnesses: []NodeHarness{{Name: "codex", Version: "1.2\x1b"}, {Name: "codex", Version: "other"}}, Resources: NodeResources{Status: "current", ObservedAt: &at, RAM: &NodeRAM{TotalBytes: 10, AvailableBytes: 11}, Agents: &NodeAgents{LiveAgents: -1}}}
	n = SanitizeNode(n)
	require.Equal(t, []string{"gpu", "test-rig"}, n.Labels)
	require.Len(t, n.Harnesses, 1)
	require.Nil(t, n.Resources.RAM)
	require.Nil(t, n.Resources.Agents)
	raw, err := json.Marshal(n)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "\\u001b")
	m, err := ParseNodeMatch("os=darwin,label=gpu,harness=codex,arch=arm64")
	require.NoError(t, err)
	require.True(t, m.Matches(n))
	m, err = ParseNodeMatch("label=missing")
	require.NoError(t, err)
	require.False(t, m.Matches(n))
	require.False(t, m.Matches(nil))
	for _, s := range []string{"os", "label=", "name=mac", "os=linux;pwd"} {
		_, err := ParseNodeMatch(s)
		require.Error(t, err)
	}
	_, err = NormalizeNodeLabels([]string{strings.Repeat("x", 65)})
	require.Error(t, err)
	require.Nil(t, SanitizeNode(&NodeMetadata{Schema: 2}))
}
