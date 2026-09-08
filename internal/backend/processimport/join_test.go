package processimport

import (
	"crypto/sha256"
	"fmt"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"testing"
)

func TestJoinExpansionPreservesIdentitiesAndConnectorPreferences(t *testing.T) {
	digest := sha256.Sum256([]byte("merge"))
	occupied := fmt.Sprintf("import_join_%x_0", digest[:12])
	source := fmt.Sprintf(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: collision
start: %s
nodes:
  %s:
    type: parallel
    next: {left: merge, right: merge}
  merge:
    type: end
    join: all
layout:
  nodes:
    merge: {x: 400, y: 300}
  edges:
    %s:
      left: {pinned: false}
      right: {pinned: true}
`, occupied, occupied, occupied)
	converted, err := Convert(source, nil)
	require.NoError(t, err)
	repeated, err := Convert(source, nil)
	require.NoError(t, err)
	require.Equal(t, converted, repeated)
	require.Len(t, converted.Process.Graph.Nodes, 3)
	require.Len(t, converted.Process.Graph.Edges, 3)
	var gate model.WorkNodeID
	for _, n := range converted.Process.Graph.Nodes {
		if n.Kind == model.WorkNodeJoin {
			gate = n.ID
			require.Equal(t, model.JoinAll, n.Join.Mode)
		}
	}
	require.NotEmpty(t, gate)
	require.NotEqual(t, model.WorkNodeID(occupied), gate)
	require.Equal(t, model.EditorPosition{X: 400, Y: 300}, converted.Layout.Nodes["merge"])
	require.Equal(t, model.EditorPosition{X: 240, Y: 300}, converted.Layout.Nodes[gate])
	require.Len(t, converted.Layout.EdgeLabels, 2)
	for _, label := range converted.Layout.EdgeLabels {
		require.Equal(t, gate, label.Edge.To)
		require.Equal(t, model.WorkNodeID(occupied), label.Edge.From)
	}
	require.False(t, converted.Layout.EdgeLabels[0].Pinned)
	require.True(t, converted.Layout.EdgeLabels[1].Pinned)
	require.Equal(t, source, converted.Source)
}
