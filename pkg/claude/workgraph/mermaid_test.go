package workgraph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// edgeSet renders edges as "from->to|label" for order-independent comparison.
func edgeSet(edges []Edge) map[string]bool {
	out := map[string]bool{}
	for _, e := range edges {
		out[e.From+"->"+e.To+"|"+e.Label] = true
	}
	return out
}

func TestParseMermaid_BasicEdgeAndDirection(t *testing.T) {
	dir, nodes, edges, err := parseMermaid("flowchart LR\n  A --> B\n")
	require.NoError(t, err)
	assert.Equal(t, "LR", dir)
	assert.Contains(t, nodes, "A")
	assert.Contains(t, nodes, "B")
	assert.Equal(t, map[string]bool{"A->B|": true}, edgeSet(edges))
}

func TestParseMermaid_DefaultDirection(t *testing.T) {
	dir, _, _, err := parseMermaid("graph\n A --> B")
	require.NoError(t, err)
	assert.Equal(t, "TD", dir)
}

func TestParseMermaid_PipeLabel(t *testing.T) {
	_, _, edges, err := parseMermaid("flowchart TD\n review -->|approved| deploy")
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"review->deploy|approved": true}, edgeSet(edges))
}

func TestParseMermaid_Chain(t *testing.T) {
	_, _, edges, err := parseMermaid("flowchart TD\n A --> B --> C")
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"A->B|": true, "B->C|": true}, edgeSet(edges))
}

func TestParseMermaid_MultiTargetAmpersand(t *testing.T) {
	_, _, edges, err := parseMermaid("flowchart TD\n A --> B & C\n D & E --> F")
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{
		"A->B|": true, "A->C|": true,
		"D->F|": true, "E->F|": true,
	}, edgeSet(edges))
}

func TestParseMermaid_Shapes(t *testing.T) {
	_, nodes, _, err := parseMermaid("flowchart TD\n A[Rect text] --> B{Diamond}\n C((Circle)) --> D([Stadium])")
	require.NoError(t, err)
	assert.Equal(t, "Rect text", nodes["A"].Text)
	assert.Equal(t, "rect", nodes["A"].Shape)
	assert.Equal(t, "diamond", nodes["B"].Shape)
	assert.Equal(t, "circle", nodes["C"].Shape)
	assert.Equal(t, "stadium", nodes["D"].Shape)
	assert.Equal(t, "Circle", nodes["C"].Text)
}

func TestParseMermaid_ShapeTextWithDashes(t *testing.T) {
	// A dash inside bracketed text must not be parsed as an edge operator.
	_, nodes, edges, err := parseMermaid("flowchart TD\n A[build a-b-c] --> B")
	require.NoError(t, err)
	assert.Equal(t, "build a-b-c", nodes["A"].Text)
	assert.Equal(t, map[string]bool{"A->B|": true}, edgeSet(edges))
}

func TestParseMermaid_FirstDeclWinsForText(t *testing.T) {
	_, nodes, _, err := parseMermaid("flowchart TD\n A[Label] --> B\n B --> A")
	require.NoError(t, err)
	assert.Equal(t, "Label", nodes["A"].Text)
}

func TestParseMermaid_CommentsAndIgnoredLines(t *testing.T) {
	src := `flowchart TD
  %% this is a comment
  subgraph cluster
  A --> B
  end
  classDef done fill:#0f0;
  class A done;
  style B fill:#f00
  A --> C`
	_, nodes, edges, err := parseMermaid(src)
	require.NoError(t, err)
	assert.Contains(t, nodes, "A")
	assert.Equal(t, map[string]bool{"A->B|": true, "A->C|": true}, edgeSet(edges))
}

func TestParseMermaid_SemicolonSeparated(t *testing.T) {
	_, _, edges, err := parseMermaid("flowchart TD\n A-->B; B-->C")
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"A->B|": true, "B->C|": true}, edgeSet(edges))
}

func TestParseMermaid_OperatorVariants(t *testing.T) {
	_, _, edges, err := parseMermaid("flowchart TD\n A --- B\n C -.-> D\n E ==> F\n G --x H")
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{
		"A->B|": true, "C->D|": true, "E->F|": true, "G->H|": true,
	}, edgeSet(edges))
}

func TestParseMermaid_SingleNodeNoEdges(t *testing.T) {
	_, nodes, edges, err := parseMermaid("flowchart TD\n only[Only node]")
	require.NoError(t, err)
	assert.Contains(t, nodes, "only")
	assert.Empty(t, edges)
}

func TestParseMermaid_Errors(t *testing.T) {
	cases := map[string]string{
		"no header":            "A --> B",
		"empty":                "",
		"reversed arrow":       "flowchart TD\n A <-- B",
		"bidirectional arrow":  "flowchart TD\n A <--> B",
		"unterminated label":   "flowchart TD\n A -->|oops B",
		"edge without target":  "flowchart TD\n A -->",
		"missing id before op": "flowchart TD\n --> B",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := parseMermaid(src)
			assert.Error(t, err, "expected %q to fail to parse", src)
		})
	}
}
