package workgraph

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for findings from the PR #226 cold review.

// A node id that merely *starts with* a mermaid keyword must keep its edges
// (the old prefix-match silently dropped these statements).
func TestParseMermaid_KeywordPrefixNodeIDsKeepEdges(t *testing.T) {
	src := `flowchart TD
  endNode --> a
  subgraphX --> b
  classDefault --> c
  styleX --> d`
	_, nodes, edges, err := parseMermaid(src)
	require.NoError(t, err)
	es := edgeSet(edges)
	assert.True(t, es["endNode->a|"])
	assert.True(t, es["subgraphX->b|"])
	assert.True(t, es["classDefault->c|"])
	assert.True(t, es["styleX->d|"])
	assert.Contains(t, nodes, "endNode")
}

// At depth 0 "end" is an ordinary node id, so its edges must survive.
func TestParseMermaid_EndAsNodeAtTopLevel(t *testing.T) {
	_, _, edges, err := parseMermaid("flowchart TD\n impl --> end\n end --> plan")
	require.NoError(t, err)
	es := edgeSet(edges)
	assert.True(t, es["impl->end|"])
	assert.True(t, es["end->plan|"], "the end->plan back-edge must not be dropped")
}

// Inside a subgraph, "end" closes it and is consumed; inner edges still parse.
func TestParseMermaid_SubgraphEndConsumed(t *testing.T) {
	src := `flowchart TD
  subgraph cluster
    a --> b
  end
  b --> c`
	_, nodes, edges, err := parseMermaid(src)
	require.NoError(t, err)
	es := edgeSet(edges)
	assert.True(t, es["a->b|"])
	assert.True(t, es["b->c|"])
	assert.NotContains(t, nodes, "end", "the subgraph terminator must not become a node")
}

// Mermaid lengthens links with extra dashes/equals; all must parse.
func TestParseMermaid_MultiDashLinks(t *testing.T) {
	src := `flowchart TD
  A ---> B
  C ----> D
  E ==> F
  G ===> H
  I ---- J`
	_, _, edges, err := parseMermaid(src)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{
		"A->B|": true, "C->D|": true, "E->F|": true, "G->H|": true, "I->J|": true,
	}, edgeSet(edges))
}

func TestParseMermaid_MultiDashWithLabel(t *testing.T) {
	_, _, edges, err := parseMermaid("flowchart TD\n A --->|yes| B")
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"A->B|yes": true}, edgeSet(edges))
}

func TestParseMermaid_ReverseArrowMidChainRejected(t *testing.T) {
	_, _, _, err := parseMermaid("flowchart TD\n A --> B <-- C")
	assert.Error(t, err)
}

func TestLoadFS_DuplicateNodeFile(t *testing.T) {
	fsys := fstest.MapFS{
		"workgraph.yaml": &fstest.MapFile{Data: []byte("name: t\n")},
		"flow.mmd":      &fstest.MapFile{Data: []byte("flowchart TD\n a --> b\n")},
		"nodes/a.yaml":  &fstest.MapFile{Data: []byte("executor: {kind: human}\n")},
		"nodes/a.yml":   &fstest.MapFile{Data: []byte("executor: {kind: human}\n")},
		"nodes/b.yaml":  &fstest.MapFile{Data: []byte("executor: {kind: human}\n")},
	}
	_, err := LoadFS(fsys, "dup", SourceUser, "")
	require.Error(t, err)
	var ve *ValidationError
	require.ErrorAs(t, err, &ve)
	found := false
	for _, p := range ve.Problems {
		if strings.Contains(p, "duplicate node definition") {
			found = true
		}
	}
	assert.True(t, found, "expected a duplicate-node problem, got: %v", ve.Problems)
}

// An enum value with no outgoing edge is a terminal outcome, not an error.
func TestLoadFS_EnumValueWithoutEdgeAllowed(t *testing.T) {
	fsys := tmplFS(
		"name: t\nentry: r\n",
		"flowchart TD\n r{R} -->|a| x\n",
		map[string]string{
			"r": "executor: {kind: human}\nverify:\n  kind: enum\n  values: [a, b]\n",
			"x": "executor: {kind: human}\n",
		},
	)
	_, err := LoadFS(fsys, "t", SourceUser, "")
	require.NoError(t, err)
}

func TestResolve_RejectsPathTraversal(t *testing.T) {
	_, err := Resolve("project:../escape", t.TempDir())
	assert.Error(t, err)
	_, err = Resolve("user:../../foo")
	assert.Error(t, err)
}
