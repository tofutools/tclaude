package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestNormalizedGraphCardinalityFullFanoutFixture(t *testing.T) {
	tmpl := maximumFanoutTemplate()
	edges, cardinalityDiagnostics := NormalizeEdgesWithinBudget(tmpl)
	require.Empty(t, cardinalityDiagnostics)
	assert.Len(t, tmpl.Nodes, MaxNormalizedNodes)
	assert.Len(t, edges, 2*MaxNormalizedDegree+1)
	assert.Equal(t, 4_093, len(edges))

	diagnostics := Validate(tmpl, edges)
	assert.False(t, diagnostics.HasErrors(), "maximum fan-out fixture diagnostics: %#v", diagnostics.Errors())
}

func TestNormalizedGraphCardinalityExactBoundaries(t *testing.T) {
	t.Run("nodes", func(t *testing.T) {
		tmpl := templateWithNodeCount(MaxNormalizedNodes)
		assert.Empty(t, PreflightNormalizedGraphCardinality(tmpl))
		edges, diagnostics := NormalizeEdgesWithinBudget(tmpl)
		assert.Empty(t, diagnostics)
		assert.Len(t, edges, 1)
	})

	t.Run("edges", func(t *testing.T) {
		tmpl := templateWithEdgeCount(MaxNormalizedEdges)
		assert.Empty(t, PreflightNormalizedGraphCardinality(tmpl))
		edges, diagnostics := NormalizeEdgesWithinBudget(tmpl)
		assert.Empty(t, diagnostics)
		assert.Len(t, edges, MaxNormalizedEdges)
	})
}

func TestNormalizedGraphCardinalityBoundaryPlusOne(t *testing.T) {
	tests := []struct {
		name      string
		tmpl      *Template
		wantCodes []string
	}{
		{"nodes", templateWithNodeCount(MaxNormalizedNodes + 1), []string{DiagnosticCodeNormalizedNodeLimit}},
		{"edges", templateWithEdgeCount(MaxNormalizedEdges + 1), []string{DiagnosticCodeNormalizedEdgeLimit}},
		{"both", templateOverBothCardinalityLimits(), []string{DiagnosticCodeNormalizedNodeLimit, DiagnosticCodeNormalizedEdgeLimit}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			diagnostics := PreflightNormalizedGraphCardinality(test.tmpl)
			assert.Equal(t, test.wantCodes, diagnosticCodes(diagnostics))
			edges, normalizedDiagnostics := NormalizeEdgesWithinBudget(test.tmpl)
			assert.Nil(t, edges, "preflight must reject before allocating normalized edges")
			assert.Equal(t, test.wantCodes, diagnosticCodes(normalizedDiagnostics))
		})
	}
}

func TestValidateRejectsCardinalityBeforeGraphWideValidation(t *testing.T) {
	tmpl := templateWithNodeCount(MaxNormalizedNodes + 1)
	// The empty header, invalid node types, and oversized edge slice would all
	// produce downstream findings if cardinality were not the first step.
	diagnostics := Validate(tmpl, make([]Edge, MaxNormalizedEdges+1))
	assert.Equal(t,
		[]string{DiagnosticCodeNormalizedNodeLimit, DiagnosticCodeNormalizedEdgeLimit},
		diagnosticCodes(diagnostics),
	)
	assert.Len(t, diagnostics, 2)
}

func TestGraphWideCanonicalizationRefusesDirectOverBudgetTemplate(t *testing.T) {
	tmpl := templateOverBothCardinalityLimits()
	for _, call := range []struct {
		name string
		run  func() (int, error)
	}{
		{"semantic hash", func() (int, error) { value, err := SemanticHash(tmpl); return len(value), err }},
		{"semantic JSON", func() (int, error) { value, err := CanonicalSemanticJSON(tmpl); return len(value), err }},
		{"canonical YAML", func() (int, error) { value, err := CanonicalYAML(tmpl); return len(value), err }},
	} {
		t.Run(call.name, func(t *testing.T) {
			resultSize, err := call.run()
			assert.Zero(t, resultSize, "budget rejection must not return partial canonical output")
			assert.ErrorIs(t, err, ErrNormalizedGraphBudget)
			var budgetErr *NormalizedGraphBudgetError
			require.ErrorAs(t, err, &budgetErr)
			assert.Equal(t,
				[]string{DiagnosticCodeNormalizedNodeLimit, DiagnosticCodeNormalizedEdgeLimit},
				diagnosticCodes(budgetErr.Diagnostics),
			)
		})
	}
}

func BenchmarkParseAliasedEdgeBoundaryPlusOne(b *testing.B) {
	source := aliasedNextTemplateYAML(64, 64, false)
	b.ReportAllocs()
	b.SetBytes(int64(len(source)))
	for range b.N {
		parsed, err := Parse(source)
		if err != nil || len(parsed.Diagnostics) != 1 || parsed.Diagnostics[0].Code != DiagnosticCodeNormalizedEdgeLimit {
			b.Fatalf("unexpected rejection: parsed=%#v err=%v", parsed, err)
		}
	}
}

func BenchmarkSchemaDiagnosticsAliasedNodeSaturation(b *testing.B) {
	source := schemaAliasedNodeTemplateYAML(MaxNormalizedNodes, 4, 1)
	b.ReportAllocs()
	b.SetBytes(int64(len(source)))
	for range b.N {
		parsed, err := Parse(source)
		if err != nil || !parsed.Diagnostics.HasNormalizedGraphBudgetError() {
			b.Fatalf("unexpected schema result: parsed=%#v err=%v", parsed, err)
		}
	}
}

func TestParseRejectsCompactAliasedNextMapsBeforeDecodeAndHash(t *testing.T) {
	t.Run("exact edge boundary", func(t *testing.T) {
		source := aliasedNextTemplateYAML(63, 65, false) // 63*65 + start = 4,096.
		parsed, err := Parse(source)
		require.NoError(t, err)
		require.NotNil(t, parsed.Template)
		assert.Len(t, parsed.Edges, MaxNormalizedEdges)
		assert.NotEmpty(t, parsed.SemanticHash)
		assert.NotContains(t, diagnosticCodes(parsed.Diagnostics), DiagnosticCodeNormalizedEdgeLimit)
	})

	t.Run("edge boundary plus one", func(t *testing.T) {
		source := aliasedNextTemplateYAML(64, 64, false) // 64*64 + start = 4,097.
		require.Less(t, len(source), 4<<20, "the source-size cap alone admits this graph")
		parsed, err := Parse(source)
		require.NoError(t, err)
		assert.Nil(t, parsed.Template, "raw guard must reject before alias materialization")
		assert.Nil(t, parsed.Edges)
		assert.Empty(t, parsed.SemanticHash, "semantic hashing is graph-wide downstream work")
		assert.Equal(t, []string{DiagnosticCodeNormalizedEdgeLimit}, diagnosticCodes(parsed.Diagnostics))
		assert.Contains(t, parsed.Diagnostics[0].Message, "counted at least 4097")
	})

	t.Run("exact edge boundary with null start", func(t *testing.T) {
		for _, test := range []struct{ name, value string }{
			{"null", "null"}, {"tilde", "~"}, {"empty", ""}, {"quoted empty", `""`}, {"explicit null", "!!null null"},
		} {
			t.Run(test.name, func(t *testing.T) {
				source := strings.Replace(string(aliasedNextTemplateYAML(64, 64, false)), "start: n000", "start: "+test.value, 1)
				parsed, err := Parse([]byte(source))
				require.NoError(t, err)
				require.NotNil(t, parsed.Template)
				assert.Empty(t, parsed.Template.Start)
				assert.Len(t, parsed.Edges, MaxNormalizedEdges)
				assert.NotContains(t, diagnosticCodes(parsed.Diagnostics), DiagnosticCodeNormalizedEdgeLimit)
				assert.Contains(t, diagnosticCodes(parsed.Diagnostics), "missing_start")
			})
		}
	})

	t.Run("null start boundary plus one", func(t *testing.T) {
		source := strings.Replace(string(aliasedNextTemplateYAML(64, 64, false)), "start: n000", "start: null", 1)
		source += "  extra:\n    type: decision\n    next:\n      pass: n000\n"
		parsed, err := Parse([]byte(source))
		require.NoError(t, err)
		assert.Nil(t, parsed.Template)
		assert.Equal(t, []string{DiagnosticCodeNormalizedEdgeLimit}, diagnosticCodes(parsed.Diagnostics))
	})

	t.Run("explicit string null is non-empty", func(t *testing.T) {
		source := strings.Replace(string(aliasedNextTemplateYAML(64, 64, false)), "start: n000", "start: !!str null", 1)
		parsed, err := Parse([]byte(source))
		require.NoError(t, err)
		assert.Nil(t, parsed.Template)
		assert.Equal(t, []string{DiagnosticCodeNormalizedEdgeLimit}, diagnosticCodes(parsed.Diagnostics))
	})

	t.Run("large alias amplification saturates", func(t *testing.T) {
		parsed, err := Parse(aliasedNextTemplateYAML(200, 200, false))
		require.NoError(t, err)
		require.Len(t, parsed.Diagnostics, 1)
		assert.Equal(t, DiagnosticCodeNormalizedEdgeLimit, parsed.Diagnostics[0].Code)
		assert.Equal(t,
			"normalized edge count exceeds 4096 (counted at least 4097, including the synthetic start edge when present)",
			parsed.Diagnostics[0].Message,
		)
	})

	t.Run("aliased structural field keys cannot hide overflow", func(t *testing.T) {
		source := string(aliasedNextTemplateYAML(64, 64, false))
		source = strings.Replace(source,
			"id: aliases\nstart:",
			"id: aliases\ndescription: &nodesKey nodes\ndoc: &nextKey next\nstart:", 1)
		source = strings.Replace(source, "nodes:\n", "*nodesKey:\n", 1)
		source = strings.ReplaceAll(source, "    next: ", "    *nextKey: ")
		parsed, err := Parse([]byte(source))
		require.NoError(t, err)
		assert.Nil(t, parsed.Template)
		assert.Equal(t, []string{DiagnosticCodeNormalizedEdgeLimit}, diagnosticCodes(parsed.Diagnostics))
	})

	t.Run("invalid branch does not hide independent overflow", func(t *testing.T) {
		parsed, err := Parse(aliasedNextTemplateYAML(64, 64, true))
		require.NoError(t, err)
		assert.Equal(t, []string{DiagnosticCodeNormalizedEdgeLimit}, diagnosticCodes(parsed.Diagnostics))
	})

	t.Run("unsupported next merge cannot bypass explicit edge counting", func(t *testing.T) {
		source := string(aliasedNextTemplateYAML(64, 64, false))
		source = strings.Replace(source, "    next: &shared\n", "    metadata:\n      defaults: &defaults\n        ignored: n000\n    next: &shared\n      <<: *defaults\n", 1)
		parsed, err := Parse([]byte(source))
		require.NoError(t, err)
		// Cardinality has deliberate precedence over alias-expanding schema
		// diagnostics. The schema walk must not emit one merge finding per
		// reference before the saturating guard rejects the graph.
		assert.Equal(t, []string{DiagnosticCodeNormalizedEdgeLimit}, diagnosticCodes(parsed.Diagnostics))
		assert.Nil(t, parsed.Template)
	})
}

func TestRawGraphScalarNextParityAndMixedBoundaries(t *testing.T) {
	t.Run("mixed mapping and scalar exact boundary", func(t *testing.T) {
		parsed, err := Parse(mixedScalarNextTemplateYAML(63, 64, 64))
		require.NoError(t, err)
		require.NotNil(t, parsed.Template)
		assert.Len(t, parsed.Edges, MaxNormalizedEdges)
		assert.NotContains(t, diagnosticCodes(parsed.Diagnostics), DiagnosticCodeNormalizedEdgeLimit)
	})

	t.Run("mixed mapping and scalar boundary plus one", func(t *testing.T) {
		parsed, err := Parse(mixedScalarNextTemplateYAML(63, 64, 65))
		require.NoError(t, err)
		assert.Nil(t, parsed.Template, "raw scalar charging must reject before Decode")
		assert.Nil(t, parsed.Edges)
		assert.Empty(t, parsed.SemanticHash)
		assert.Equal(t, []string{DiagnosticCodeNormalizedEdgeLimit}, diagnosticCodes(parsed.Diagnostics))
	})

	t.Run("scalar-only exact and boundary-plus-one raw counts", func(t *testing.T) {
		for _, count := range []int{MaxNormalizedEdges, MaxNormalizedEdges + 1} {
			root := rawGraphDocumentWithScalarNext(count)
			counts, status, diagnostics := rawNormalizedGraphCardinality(root)
			assert.Equal(t, rawGraphCounted, status)
			assert.Empty(t, diagnostics)
			assert.Equal(t, count, counts.Edges)
		}
	})

	for _, test := range []struct {
		name, scalar string
		wantEdges    int
	}{
		{"null", "null", 0}, {"tilde", "~", 0}, {"empty", "", 0},
		{"quoted empty", `""`, 0}, {"explicit null", "!!null null", 0},
		{"target", "target", 1}, {"explicit string null", "!!str null", 1},
	} {
		t.Run("decoded scalar/"+test.name, func(t *testing.T) {
			source := []byte("apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: scalar\nstart: null\nnodes:\n  source:\n    type: decision\n    next: " + test.scalar + "\n  target: {type: end}\n")
			parsed, err := Parse(source)
			require.NoError(t, err)
			require.NotNil(t, parsed.Template)
			assert.Len(t, parsed.Edges, test.wantEdges)
		})
	}

	t.Run("aliased tagged scalar", func(t *testing.T) {
		source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: scalar-alias
description: &target !!str null
start: null
nodes:
  source: {type: decision, next: *target}
  target: {type: end}
`)
		parsed, err := Parse(source)
		require.NoError(t, err)
		require.NotNil(t, parsed.Template)
		assert.Len(t, parsed.Edges, 1)
		assert.Equal(t, "null", parsed.Template.Nodes["source"].Next[DefaultOutcome])
	})

	for _, aliasKind := range []string{"cycle", "depth"} {
		for _, edgeCount := range []int{MaxNormalizedEdges, MaxNormalizedEdges + 1} {
			name := "exact"
			if edgeCount > MaxNormalizedEdges {
				name = "boundary-plus-one"
			}
			t.Run("scalar alias "+aliasKind+"/"+name, func(t *testing.T) {
				root, _ := rawGraphDocumentWithSharedNext(edgeCount, 1)
				installUnsafeScalarNext(root, aliasKind)
				counts, status, diagnostics := rawNormalizedGraphCardinality(root)
				assert.Equal(t, edgeCount, counts.Edges)
				assert.Equal(t, rawGraphRejected, status)
				require.Len(t, diagnostics, 1)
				assert.Equal(t, DiagnosticCodeGraphAliasLimit, diagnostics[0].Code)
				assert.Equal(t, "nodes.scalar-alias.next", diagnostics[0].Path)
				if edgeCount > MaxNormalizedEdges {
					assert.Equal(t, []string{DiagnosticCodeNormalizedEdgeLimit},
						diagnosticCodes(normalizedGraphCardinalityDiagnostics(counts)))
				} else {
					assert.Empty(t, normalizedGraphCardinalityDiagnostics(counts))
				}
			})
		}
	}

	for _, independentEdges := range []int{MaxNormalizedEdges - 1, MaxNormalizedEdges} {
		name := "exact"
		if independentEdges == MaxNormalizedEdges {
			name = "boundary-plus-one"
		}
		t.Run("scalar charge survives malformed node key/"+name, func(t *testing.T) {
			root, _ := rawGraphDocumentWithSharedNext(independentEdges, 1)
			nodes := root.Content[0].Content[1]
			nodes.Content = append(nodes.Content,
				&yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: "malformed"}}},
				&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
					{Kind: yaml.ScalarNode, Value: "next"}, {Kind: yaml.ScalarNode, Value: "target"},
				}},
			)
			counts, status, diagnostics := rawNormalizedGraphCardinality(root)
			assert.Equal(t, independentEdges+1, counts.Edges)
			assert.Equal(t, rawGraphRejected, status)
			require.Len(t, diagnostics, 1)
			assert.Equal(t, DiagnosticCodeInvalidGraphKey, diagnostics[0].Code)
			if independentEdges == MaxNormalizedEdges {
				assert.Equal(t, []string{DiagnosticCodeNormalizedEdgeLimit},
					diagnosticCodes(normalizedGraphCardinalityDiagnostics(counts)))
			} else {
				assert.Empty(t, normalizedGraphCardinalityDiagnostics(counts))
			}
		})
	}

	t.Run("duplicate node last-wins scalar authority", func(t *testing.T) {
		source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: scalar-last-wins
start: null
nodes:
  source: {type: decision, next: {pass: target, fail: target}}
  source: {type: decision, next: target}
  target: {type: end}
`)
		parsed, err := Parse(source)
		require.NoError(t, err)
		require.NotNil(t, parsed.Template)
		assert.Len(t, parsed.Edges, 1)
		assert.Contains(t, diagnosticCodes(parsed.Diagnostics), "duplicate_key")
	})

	t.Run("JSON scalar parity", func(t *testing.T) {
		source := []byte(`{"apiVersion":"tclaude.dev/v1alpha1","kind":"ProcessTemplate","id":"scalar-json","start":null,"nodes":{"source":{"type":"decision","next":"target"},"target":{"type":"end"}}}`)
		parsed, err := Parse(source)
		require.NoError(t, err)
		require.NotNil(t, parsed.Template)
		assert.Len(t, parsed.Edges, 1)
	})
}

func TestRawGraphMalformedKeysFailClosedWithoutHidingCardinality(t *testing.T) {
	assertRejected := func(t *testing.T, source []byte, wantCode, wantPath string) {
		t.Helper()
		parsed, err := Parse(source)
		require.NoError(t, err)
		assert.Nil(t, parsed.Template, "predecode rejection must not materialize Template maps")
		assert.Nil(t, parsed.Edges, "predecode rejection must not normalize the graph")
		assert.Empty(t, parsed.SemanticHash, "predecode rejection must not hash the graph")
		require.NotEmpty(t, parsed.Diagnostics)
		assert.Equal(t, wantCode, parsed.Diagnostics[len(parsed.Diagnostics)-1].Code)
		assert.Equal(t, wantPath, parsed.Diagnostics[len(parsed.Diagnostics)-1].Path)
	}

	for _, graphKey := range []struct {
		name    string
		rewrite func(string, int) string
		path    func(int) string
	}{
		{"node key", replaceNodeKeyWithComplex, func(int) string { return "nodes" }},
		{"outcome key", replaceOutcomeKeyWithComplex, func(nodes int) string {
			return fmt.Sprintf("nodes.n%03d.next", nodes-1)
		}},
	} {
		t.Run(graphKey.name, func(t *testing.T) {
			t.Run("under boundary", func(t *testing.T) {
				source := graphKey.rewrite(string(aliasedNextTemplateYAML(1, 1, false)), 0)
				assertRejected(t, []byte(source), DiagnosticCodeInvalidGraphKey, graphKey.path(1))
			})

			t.Run("exact edge boundary", func(t *testing.T) {
				source := graphKey.rewrite(string(aliasedNextTemplateYAML(63, 65, false)), 0)
				assertRejected(t, []byte(source), DiagnosticCodeInvalidGraphKey, graphKey.path(63))
			})

			t.Run("edge boundary plus one before malformed key", func(t *testing.T) {
				source := graphKey.rewrite(string(aliasedNextTemplateYAML(64, 64, false)), 0)
				assertRejected(t, []byte(source), DiagnosticCodeNormalizedEdgeLimit, "nodes")
			})

			t.Run("edge boundary plus one after malformed key", func(t *testing.T) {
				source := graphKey.rewrite(string(aliasedNextTemplateYAML(64, 64, false)), 63)
				assertRejected(t, []byte(source), DiagnosticCodeNormalizedEdgeLimit, "nodes")
			})
		})
	}

	t.Run("worst allowed node and degree amplification", func(t *testing.T) {
		source := replaceNodeKeyWithComplex(
			string(aliasedNextTemplateYAML(MaxNormalizedNodes, MaxNormalizedDegree, false)),
			MaxNormalizedNodes-1,
		)
		require.Less(t, len(source), MaxProcessTemplateSourceBytes)
		assertRejected(t, []byte(source), DiagnosticCodeNormalizedEdgeLimit, "nodes")
	})

	t.Run("malformed node field key still traverses next", func(t *testing.T) {
		source := string(aliasedNextTemplateYAML(64, 64, false))
		source = strings.Replace(source, "    type: end\n    next: &shared", "    ? [bad-field]\n    : ignored\n    next: &shared", 1)
		assertRejected(t, []byte(source), DiagnosticCodeNormalizedEdgeLimit, "nodes")
	})

	t.Run("multiple malformed keys emit one bounded deterministic finding", func(t *testing.T) {
		source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: malformed-keys
description: &bad-one [one]
metadata: &bad-two [two]
nodes:
  *bad-one: {type: end}
  *bad-two: {type: end}
`)
		parsed, err := Parse(source)
		require.NoError(t, err)
		assert.Nil(t, parsed.Template)
		require.NotEmpty(t, parsed.Diagnostics)
		assert.Equal(t, DiagnosticCodeInvalidGraphKey, parsed.Diagnostics[len(parsed.Diagnostics)-1].Code)
		assert.Equal(t, "nodes", parsed.Diagnostics[len(parsed.Diagnostics)-1].Path)
		encoded, err := json.Marshal(parsed.Diagnostics)
		require.NoError(t, err)
		assert.Less(t, len(encoded), 1024)
		assert.Equal(t, 1, countDiagnosticCode(parsed.Diagnostics, DiagnosticCodeInvalidGraphKey))
	})

	t.Run("valid duplicate last wins before malformed-key rejection", func(t *testing.T) {
		source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: malformed-with-duplicate
start: target
nodes:
  source:
    type: decision
    next:
      pass: missing
      pass: target
      ? [malformed-outcome]
      : target
  target: {type: end}
`)
		parsed, err := Parse(source)
		require.NoError(t, err)
		assert.Nil(t, parsed.Template)
		assert.Equal(t,
			[]string{"duplicate_key", DiagnosticCodeInvalidGraphKey},
			diagnosticCodes(parsed.Diagnostics),
		)
		assert.Equal(t, "nodes.source.next", parsed.Diagnostics[1].Path)
	})

	t.Run("independent overflow takes precedence over malformed node merge", func(t *testing.T) {
		source := string(aliasedNextTemplateYAML(64, 64, false))
		source = strings.Replace(source, "  n063:\n    type: end", "  n063:\n    <<: &node-defaults {type: end}\n    type: end", 1)
		assertRejected(t, []byte(source), DiagnosticCodeNormalizedEdgeLimit, "nodes")
	})

	t.Run("malformed key does not hide node overflow", func(t *testing.T) {
		var source strings.Builder
		source.WriteString("apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: malformed-node-overflow\nnodes:\n")
		for index := 0; index < MaxNormalizedNodes+1; index++ {
			fmt.Fprintf(&source, "  n%04d: {type: end}\n", index)
		}
		malformed := strings.Replace(source.String(), "  n0000: {type: end}\n",
			"  ? [malformed-node-key]\n  : {type: end}\n", 1)
		assertRejected(t, []byte(malformed), DiagnosticCodeNormalizedNodeLimit, "nodes")
	})
}

func TestParseResolvesAliasedStructuralGraphKeysWithinFiniteBound(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		wantNodes int
		wantEdges int
	}{
		{
			name: "root nodes key",
			source: `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: aliases
description: &nodesKey nodes
*nodesKey:
  one: {type: end}
`,
			wantNodes: 1,
		},
		{
			name: "node next key",
			source: `apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: aliases
start: one
nodes:
  one:
    type: end
    description: &nextKey next
    *nextKey:
      pass: one
`,
			wantNodes: 1,
			wantEdges: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var root yaml.Node
			require.NoError(t, yaml.Unmarshal([]byte(test.source), &root))
			var decoded Template
			require.NoError(t, root.Decode(&decoded), "the existing decoder accepts scalar aliases as mapping keys")

			parsed, err := Parse([]byte(test.source))
			require.NoError(t, err)
			require.NotNil(t, parsed.Template)
			assert.Len(t, parsed.Template.Nodes, test.wantNodes)
			assert.Len(t, parsed.Edges, test.wantEdges)
			assert.NotContains(t, diagnosticCodes(parsed.Diagnostics), DiagnosticCodeGraphAliasLimit)
		})
	}
}

func TestRawGraphKeyIdentityMatchesDecodedNullLastWins(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: null-keys
start: target
nodes:
  source:
    type: decision
    next:
      null: target
      ~: target
  target: {type: end}
`)
	parsed, err := Parse(source)
	require.NoError(t, err)
	require.NotNil(t, parsed.Template)
	assert.Len(t, parsed.Template.Nodes["source"].Next, 1)
	assert.Len(t, parsed.Edges, 2, "one start edge plus one decoded empty-outcome edge")
	assert.NotContains(t, diagnosticCodes(parsed.Diagnostics), DiagnosticCodeNormalizedEdgeLimit)
}

func TestRawGraphNullKeyIdentityAtExactEdgeBoundary(t *testing.T) {
	source := strings.Replace(string(aliasedNextTemplateYAML(64, 64, false)), "start: n000", "start: null", 1)
	source = strings.Replace(source, "      outcome-000: n000\n", "      null: n000\n", 1)
	source = strings.Replace(source, "      outcome-001: n000\n", "      ~: n000\n", 1)
	source = strings.Replace(source, "  n001:\n", "      extra: n000\n  n001:\n", 1)
	parsed, err := Parse([]byte(source))
	require.NoError(t, err)
	require.NotNil(t, parsed.Template)
	assert.Len(t, parsed.Edges, MaxNormalizedEdges)
	assert.NotContains(t, diagnosticCodes(parsed.Diagnostics), DiagnosticCodeNormalizedEdgeLimit)
	assert.Contains(t, diagnosticCodes(parsed.Diagnostics), "duplicate_key")
	assert.True(t, hasDiagnosticPath(parsed.Diagnostics, "duplicate_key", "nodes.n000.next."))

	source += "  extra:\n    type: decision\n    next:\n      pass: n000\n"
	parsed, err = Parse([]byte(source))
	require.NoError(t, err)
	assert.Nil(t, parsed.Template)
	assert.Equal(t, []string{"duplicate_key", DiagnosticCodeNormalizedEdgeLimit}, diagnosticCodes(parsed.Diagnostics))
}

func TestDecodedStructuralScalarMatchesYAMLStringDecode(t *testing.T) {
	for _, test := range []struct{ name, scalar string }{
		{"null", "null"}, {"tilde", "~"}, {"empty", ""}, {"quoted empty", `""`},
		{"explicit null", "!!null null"}, {"explicit string null", "!!str null"},
		{"integer", "42"}, {"boolean", "true"}, {"quoted integer", `"42"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var root yaml.Node
			require.NoError(t, yaml.Unmarshal([]byte("value: "+test.scalar+"\n"), &root))
			node := root.Content[0].Content[1]
			var decoded string
			require.NoError(t, node.Decode(&decoded))
			resolved, ok := decodedStructuralScalar(node)
			require.True(t, ok)
			assert.Equal(t, decoded, resolved)
		})
	}

	var aliasRoot yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte("anchor: &value !!null null\nvalue: *value\n"), &aliasRoot))
	alias := aliasRoot.Content[0].Content[3]
	resolved, ok := decodedStructuralScalar(alias)
	require.True(t, ok)
	assert.Empty(t, resolved)

	for _, kind := range []yaml.Kind{yaml.MappingNode, yaml.SequenceNode} {
		_, ok := decodedStructuralScalar(&yaml.Node{Kind: kind})
		assert.False(t, ok)
	}
}

func TestRawGraphWrongKindStartDefersToDecoder(t *testing.T) {
	for _, value := range []string{"[one]", "{target: one}"} {
		source := []byte("apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: wrong-start\nstart: " + value + "\nnodes:\n  one: {type: end}\n")
		_, err := Parse(source)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "decode process template")
	}
}

func TestRawGraphNullStartJSONParityAtExactBoundary(t *testing.T) {
	tmpl := templateWithEdgeCount(MaxNormalizedEdges + 1)
	tmpl.Start = ""
	source, err := json.Marshal(tmpl)
	require.NoError(t, err)
	source = bytes.Replace(source, []byte(`"start":""`), []byte(`"start":null`), 1)
	parsed, err := Parse(source)
	require.NoError(t, err)
	require.NotNil(t, parsed.Template)
	assert.Empty(t, parsed.Template.Start)
	assert.Len(t, parsed.Edges, MaxNormalizedEdges)
	assert.NotContains(t, diagnosticCodes(parsed.Diagnostics), DiagnosticCodeNormalizedEdgeLimit)
}

func TestSchemaAliasTraversalAndDiagnosticsAreBounded(t *testing.T) {
	t.Run("large valid shared subtree is memoized", func(t *testing.T) {
		parsed, err := Parse(schemaAliasedNodeTemplateYAML(MaxNormalizedNodes, 4, 0))
		require.NoError(t, err)
		require.NotNil(t, parsed.Template)
		assert.Len(t, parsed.Template.Nodes, MaxNormalizedNodes)
		assert.NotContains(t, diagnosticCodes(parsed.Diagnostics), DiagnosticCodeDiagnosticBudget)
	})

	t.Run("unknown findings saturate with deterministic occurrence prefix", func(t *testing.T) {
		source := schemaAliasedNodeTemplateYAML(MaxNormalizedNodes, 4, 1)
		parsed, err := Parse(source)
		require.NoError(t, err)
		assert.Nil(t, parsed.Template, "schema resource rejection must stop before Decode")
		require.NotEmpty(t, parsed.Diagnostics)
		assert.LessOrEqual(t, len(parsed.Diagnostics), MaxTemplateAuthoringDiagnostics)
		assert.Equal(t, "nodes.n000.checks[0].unknown-000", parsed.Diagnostics[0].Path)
		assert.Equal(t, DiagnosticCodeDiagnosticBudget, parsed.Diagnostics[len(parsed.Diagnostics)-1].Code)
		assert.Less(t, testing.AllocsPerRun(1, func() {
			result, parseErr := Parse(source)
			if parseErr != nil || result.Template != nil {
				panic("unexpected schema saturation result")
			}
		}), float64(500_000))
	})

	t.Run("huge unknown key cannot amplify path or message", func(t *testing.T) {
		hugeKey := strings.Repeat("k", MaxTemplateDiagnosticWireBytes/2+1)
		root := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: hugeKey}, {Kind: yaml.ScalarNode, Value: "value"},
		}}
		diagnostics := unknownFieldDiagnostics(root, &templateDiagnosticBudget{})
		require.Len(t, diagnostics, 1)
		assert.Equal(t, DiagnosticCodeDiagnosticBudget, diagnostics[0].Code)
		assert.Less(t, len(diagnostics[0].Message)+len(diagnostics[0].Path), 512)
	})
}

func TestPreDecodeDiagnosticBudgetIncludesDuplicateAndSchemaFindings(t *testing.T) {
	t.Run("freeform duplicate flood fails closed", func(t *testing.T) {
		source := duplicateMetadataTemplateYAML(100_000, false)
		require.Less(t, len(source), MaxProcessTemplateSourceBytes)
		parsed, err := Parse(source)
		require.NoError(t, err)
		assert.Nil(t, parsed.Template)
		require.NotEmpty(t, parsed.Diagnostics)
		assert.LessOrEqual(t, len(parsed.Diagnostics), MaxTemplateAuthoringDiagnostics)
		assert.Equal(t, "duplicate_key", parsed.Diagnostics[0].Code)
		assert.Equal(t, DiagnosticCodeDiagnosticBudget, parsed.Diagnostics[len(parsed.Diagnostics)-1].Code)
		assert.Equal(t, 1, countDiagnosticCode(parsed.Diagnostics, DiagnosticCodeDiagnosticBudget))
	})

	t.Run("mixed findings preserve deterministic prefix", func(t *testing.T) {
		source := duplicateMetadataTemplateYAML(3, true)
		parsed, err := Parse(source)
		require.NoError(t, err)
		require.NotEmpty(t, parsed.Diagnostics)
		assert.Equal(t, []string{"duplicate_key", "duplicate_key", "unknown_field"}, diagnosticCodes(parsed.Diagnostics[:3]))
		assert.Equal(t, "nodes.n000.checks[0].unknown-000", parsed.Diagnostics[2].Path)
		assert.Equal(t, DiagnosticCodeDiagnosticBudget, parsed.Diagnostics[len(parsed.Diagnostics)-1].Code)
		assert.Equal(t, 1, countDiagnosticCode(parsed.Diagnostics, DiagnosticCodeDiagnosticBudget))
	})
}

func TestContextAwareDuplicatePruningSharesLargeUniqueAliasTree(t *testing.T) {
	shared := &yaml.Node{Kind: yaml.MappingNode}
	for i := 0; i < 10_000; i++ {
		shared.Content = append(shared.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("key-%05d", i)},
			&yaml.Node{Kind: yaml.ScalarNode, Value: "value"},
		)
	}
	param := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "default"}, shared,
	}}
	params := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "settings"}, param,
	}}
	mapping := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "params"}, params,
		{Kind: yaml.ScalarNode, Value: "nodes"}, {Kind: yaml.AliasNode, Alias: shared},
	}}
	root := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{mapping}}

	pruned := pruneDuplicateKeys(root)
	assert.Same(t, root, pruned, "unique alias tree should remain structurally shared")
	assert.Same(t, shared, pruned.Content[0].Content[1].Content[1].Content[1])
	assert.Less(t, testing.AllocsPerRun(1, func() {
		if pruneDuplicateKeys(root) != root {
			panic("unique alias tree was copied")
		}
	}), float64(220_000), "context-aware pruning must not clone the dense alias tree")
}

func TestTemplateDiagnosticBudgetExactCountBoundaryAndWireBound(t *testing.T) {
	t.Run("count", func(t *testing.T) {
		budget := &templateDiagnosticBudget{}
		var diagnostics Diagnostics
		diagnostic := diagError("x", "", "")
		for range MaxTemplateAuthoringDiagnostics - 1 {
			require.True(t, budget.append(&diagnostics, diagnostic))
		}
		assert.False(t, budget.append(&diagnostics, diagnostic), "boundary plus one must saturate")
		diagnostics = append(diagnostics, templateDiagnosticBudgetDiagnostic())
		assert.Len(t, diagnostics, MaxTemplateAuthoringDiagnostics)
		assert.Equal(t, 1, countDiagnosticCode(diagnostics, DiagnosticCodeDiagnosticBudget))
		assertDiagnosticWireBudget(t, budget)
	})

	t.Run("wire bytes", func(t *testing.T) {
		budget := &templateDiagnosticBudget{}
		var diagnostics Diagnostics
		sentinel := templateDiagnosticBudgetDiagnostic()
		ordinaryLimit := MaxTemplateDiagnosticWireBytes - templateDiagnosticWireCost(len(sentinel.Code), len(sentinel.Path), len(sentinel.Message))
		messageBytes := (ordinaryLimit-templateDiagnosticFixedWireBytes)/templateDiagnosticJSONExpansion - len("x")
		require.True(t, budget.append(&diagnostics, diagError("x", "", strings.Repeat("<", messageBytes))))
		assert.False(t, budget.append(&diagnostics, diagError("x", "", "x")), "wire boundary plus one must saturate")
		diagnostics = append(diagnostics, sentinel)
		assert.Equal(t, 1, countDiagnosticCode(diagnostics, DiagnosticCodeDiagnosticBudget))
		assertDiagnosticWireBudget(t, budget)
	})
}

func TestTemplateDiagnosticWireCostCoversJSONEscaping(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{"html", strings.Repeat("<>&", 100)},
		{"quotes backslashes and controls", strings.Repeat("\"\\\n\r\t", 100)},
		{"invalid UTF-8 replacement", string([]byte{0xff, 0xfe, 0xfd})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]string{
				"scope": "node", "targetId": test.value, "severity": "error",
				"code": "unknown_field", "message": test.value,
			})
			require.NoError(t, err)
			cost := templateDiagnosticWireCost(len("unknown_field"), len(test.value), len(test.value))
			assert.GreaterOrEqual(t, cost, len(payload))
		})
	}
}

func TestSemanticDiagnosticsShareEndToEndAuthoringBudget(t *testing.T) {
	tmpl := semanticDiagnosticFloodTemplate(100_000)
	edges, cardinalityDiagnostics := NormalizeEdgesWithinBudget(tmpl)
	require.Empty(t, cardinalityDiagnostics)

	t.Run("direct Validate", func(t *testing.T) {
		diagnostics := Validate(tmpl, edges)
		require.NotEmpty(t, diagnostics)
		assert.LessOrEqual(t, len(diagnostics), MaxTemplateAuthoringDiagnostics)
		assert.Equal(t, "undeclared_param_ref", diagnostics[0].Code)
		assert.Equal(t, DiagnosticCodeDiagnosticBudget, diagnostics[len(diagnostics)-1].Code)
		assert.Equal(t, 1, countDiagnosticCode(diagnostics, DiagnosticCodeDiagnosticBudget))
		encoded, err := json.Marshal(diagnostics)
		require.NoError(t, err)
		assert.LessOrEqual(t, len(encoded), MaxTemplateDiagnosticWireBytes)
	})

	source, err := CanonicalYAML(tmpl)
	require.NoError(t, err)
	require.Less(t, len(source), MaxProcessTemplateSourceBytes)

	t.Run("Parse stops before hash", func(t *testing.T) {
		parsed, parseErr := Parse(source)
		require.NoError(t, parseErr)
		require.NotNil(t, parsed.Template)
		assert.Empty(t, parsed.SemanticHash)
		require.NotEmpty(t, parsed.Diagnostics)
		assert.LessOrEqual(t, len(parsed.Diagnostics), MaxTemplateAuthoringDiagnostics)
		assert.Equal(t, DiagnosticCodeDiagnosticBudget, parsed.Diagnostics[len(parsed.Diagnostics)-1].Code)
		encoded, marshalErr := json.Marshal(parsed.Diagnostics)
		require.NoError(t, marshalErr)
		assert.LessOrEqual(t, len(encoded), MaxTemplateDiagnosticWireBytes)
		assert.Less(t, testing.AllocsPerRun(1, func() {
			result, runErr := Parse(source)
			if runErr != nil || result.Template == nil || !result.Diagnostics.HasNormalizedGraphBudgetError() {
				panic("unexpected semantic diagnostic saturation")
			}
		}), float64(300_000))
	})

	t.Run("predecode and semantic findings use one prefix", func(t *testing.T) {
		mixed := bytes.Replace(source, []byte("kind: ProcessTemplate\n"),
			[]byte("kind: ProcessTemplate\nname: first\nname: second\n"), 1)
		parsed, parseErr := Parse(mixed)
		require.NoError(t, parseErr)
		require.NotEmpty(t, parsed.Diagnostics)
		assert.LessOrEqual(t, len(parsed.Diagnostics), MaxTemplateAuthoringDiagnostics)
		assert.Equal(t, "duplicate_key", parsed.Diagnostics[0].Code)
		assert.Equal(t, "undeclared_param_ref", parsed.Diagnostics[1].Code)
		assert.Equal(t, DiagnosticCodeDiagnosticBudget, parsed.Diagnostics[len(parsed.Diagnostics)-1].Code)
		assert.Equal(t, 1, countDiagnosticCode(parsed.Diagnostics, DiagnosticCodeDiagnosticBudget))
	})

	t.Run("huge escaped semantic message saturates without wire amplification", func(t *testing.T) {
		huge := semanticDiagnosticFloodTemplate(0)
		huge.Nodes["source"] = Node{
			Type: NodeTypeTask,
			Performer: &Performer{Kind: PerformerAgent,
				Prompt: "{{ params." + strings.Repeat("A", MaxTemplateDiagnosticWireBytes/2) + " }}"},
			Next: Next{"pass": "target"},
		}
		hugeEdges, edgeDiagnostics := NormalizeEdgesWithinBudget(huge)
		require.Empty(t, edgeDiagnostics)
		diagnostics := Validate(huge, hugeEdges)
		require.Len(t, diagnostics, 1)
		assert.Equal(t, DiagnosticCodeDiagnosticBudget, diagnostics[0].Code)
		encoded, marshalErr := json.Marshal(diagnostics)
		require.NoError(t, marshalErr)
		assert.Less(t, len(encoded), 1024)
	})
}

func assertDiagnosticWireBudget(t *testing.T, budget *templateDiagnosticBudget) {
	t.Helper()
	sentinel := templateDiagnosticBudgetDiagnostic()
	assert.LessOrEqual(t,
		budget.wireBytes+templateDiagnosticWireCost(len(sentinel.Code), len(sentinel.Path), len(sentinel.Message)),
		MaxTemplateDiagnosticWireBytes,
	)
}

func TestRawGraphAliasInspectionFailsClosedOnWrongKindContainer(t *testing.T) {
	malformed := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: malformed
start: one
nodes:
  one:
    type: task
    next: &shared [one]
  two:
    type: task
    next: *shared
`)
	parsed, err := Parse(malformed)
	require.NoError(t, err)
	assert.Nil(t, parsed.Template)
	require.Len(t, parsed.Diagnostics, 1)
	assert.Equal(t, DiagnosticCodeInvalidGraphShape, parsed.Diagnostics[0].Code)
	assert.Equal(t, "nodes.two.next", parsed.Diagnostics[0].Path)

	cyclicValue := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: cyclic
start: one
nodes:
  one:
    type: task
    next: &shared
      pass: *shared
`)
	_, err = Parse(cyclicValue)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode process template")
}

func TestRawGraphCardinalityPreflightKeepsJSONAndOrdinaryYAMLParity(t *testing.T) {
	yamlParsed, err := Parse([]byte(validTemplateYAML))
	require.NoError(t, err)
	require.False(t, yamlParsed.Diagnostics.HasErrors())
	jsonSource, err := json.Marshal(yamlParsed.Template)
	require.NoError(t, err)
	jsonParsed, err := Parse(jsonSource)
	require.NoError(t, err)
	require.False(t, jsonParsed.Diagnostics.HasErrors())
	assert.Equal(t, yamlParsed.SemanticHash, jsonParsed.SemanticHash)
	assert.Equal(t, yamlParsed.Edges, jsonParsed.Edges)
}

func TestStructuralAliasResolutionUsesFiniteTreeBound(t *testing.T) {
	leaf := &yaml.Node{Kind: yaml.MappingNode}
	node := leaf
	const depth = 128
	for range depth {
		node = &yaml.Node{Kind: yaml.AliasNode, Alias: node}
	}
	resolved, status := structuralNode(node, depth+1)
	assert.Equal(t, rawGraphCounted, status)
	assert.Same(t, leaf, resolved)
	_, status = structuralNode(node, depth-1)
	assert.Equal(t, rawGraphAliasUnsafe, status)

	cycle := &yaml.Node{Kind: yaml.AliasNode}
	cycle.Alias = cycle
	_, status = structuralNode(cycle, 2)
	assert.Equal(t, rawGraphAliasUnsafe, status)
}

func TestRawGraphStickyAliasIssuesPreserveSaturatedCounts(t *testing.T) {
	for _, keyKind := range []string{"cycle", "depth"} {
		for _, position := range []string{"before", "after"} {
			t.Run(keyKind+"/exact/"+position, func(t *testing.T) {
				root, next := rawGraphDocumentWithSharedNext(MaxNormalizedEdges, 1)
				installUnsafeStructuralKey(root, next, position, keyKind)
				counts, status, diagnostics := rawNormalizedGraphCardinality(root)
				assert.Equal(t, MaxNormalizedEdges, counts.Edges)
				assert.Equal(t, rawGraphRejected, status)
				require.Len(t, diagnostics, 1)
				assert.Equal(t, DiagnosticCodeGraphAliasLimit, diagnostics[0].Code)
				assert.Equal(t, "nodes.n000.next", diagnostics[0].Path)
			})

			t.Run(keyKind+"/boundary-plus-one/"+position, func(t *testing.T) {
				root, next := rawGraphDocumentWithSharedNext(MaxNormalizedEdges+1, 1)
				installUnsafeStructuralKey(root, next, position, keyKind)
				counts, status, diagnostics := rawNormalizedGraphCardinality(root)
				assert.Equal(t, MaxNormalizedEdges+1, counts.Edges)
				assert.Equal(t, rawGraphRejected, status)
				assert.Equal(t, []string{DiagnosticCodeNormalizedEdgeLimit},
					diagnosticCodes(normalizedGraphCardinalityDiagnostics(counts)))
				require.Len(t, diagnostics, 1)
				assert.Equal(t, DiagnosticCodeGraphAliasLimit, diagnostics[0].Code)
			})
		}
	}

	t.Run("indeterminate container does not stop independent overflow", func(t *testing.T) {
		root, _ := rawGraphDocumentWithSharedNext(MaxNormalizedEdges+1, 1)
		nodes := root.Content[0].Content[1]
		cycle := &yaml.Node{Kind: yaml.AliasNode}
		cycle.Alias = cycle
		nodes.Content = append(nodes.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "indeterminate"}, cycle,
		)
		counts, status, diagnostics := rawNormalizedGraphCardinality(root)
		assert.Equal(t, MaxNormalizedEdges+1, counts.Edges)
		assert.Equal(t, rawGraphRejected, status)
		assert.Equal(t, []string{DiagnosticCodeNormalizedEdgeLimit},
			diagnosticCodes(normalizedGraphCardinalityDiagnostics(counts)))
		require.Len(t, diagnostics, 1)
		assert.Equal(t, DiagnosticCodeGraphAliasLimit, diagnostics[0].Code)
		assert.Equal(t, "nodes.indeterminate", diagnostics[0].Path)
	})

	t.Run("cached shared issue binds to first authoritative occurrence", func(t *testing.T) {
		root, next := rawGraphDocumentWithSharedNext(1, 2)
		installUnsafeStructuralKey(root, next, "before", "cycle")
		counts, status, diagnostics := rawNormalizedGraphCardinality(root)
		assert.Equal(t, 2, counts.Edges, "the cached local count contributes once per alias occurrence")
		assert.Equal(t, rawGraphRejected, status)
		require.Len(t, diagnostics, 1)
		assert.Equal(t, DiagnosticCodeGraphAliasLimit, diagnostics[0].Code)
		assert.Equal(t, "nodes.n001.next", diagnostics[0].Path)
	})

	t.Run("alias status does not hide node overflow", func(t *testing.T) {
		root, _ := rawGraphDocumentWithSharedNext(0, MaxNormalizedNodes+1)
		nodes := root.Content[0].Content[1]
		cycle := &yaml.Node{Kind: yaml.AliasNode}
		cycle.Alias = cycle
		nodes.Content[0] = cycle
		counts, status, diagnostics := rawNormalizedGraphCardinality(root)
		assert.Equal(t, MaxNormalizedNodes+1, counts.Nodes)
		assert.Equal(t, rawGraphRejected, status)
		assert.Equal(t, []string{DiagnosticCodeNormalizedNodeLimit},
			diagnosticCodes(normalizedGraphCardinalityDiagnostics(counts)))
		require.Len(t, diagnostics, 1)
		assert.Equal(t, DiagnosticCodeGraphAliasLimit, diagnostics[0].Code)
		encoded, err := json.Marshal(diagnostics)
		require.NoError(t, err)
		assert.Less(t, len(encoded), 1024)
	})

	for _, first := range []rawGraphStructuralIssue{rawGraphInvalidKey, rawGraphUnsafeAlias} {
		name := "invalid-first"
		if first == rawGraphUnsafeAlias {
			name = "alias-first"
		}
		t.Run("first structural issue is deterministic/"+name, func(t *testing.T) {
			root, next := rawGraphDocumentWithSharedNext(2, 1)
			cycle := &yaml.Node{Kind: yaml.AliasNode}
			cycle.Alias = cycle
			complex := &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: "bad"}}}
			if first == rawGraphInvalidKey {
				next.Content[0], next.Content[2] = complex, cycle
			} else {
				next.Content[0], next.Content[2] = cycle, complex
			}
			_, status, diagnostics := rawNormalizedGraphCardinality(root)
			assert.Equal(t, rawGraphRejected, status)
			require.Len(t, diagnostics, 1)
			wantCode := DiagnosticCodeInvalidGraphKey
			if first == rawGraphUnsafeAlias {
				wantCode = DiagnosticCodeGraphAliasLimit
			}
			assert.Equal(t, wantCode, diagnostics[0].Code)
			assert.Equal(t, "nodes.n000.next", diagnostics[0].Path)
		})
	}
}

func rawGraphDocumentWithSharedNext(outcomes, nodeCount int) (*yaml.Node, *yaml.Node) {
	next := &yaml.Node{Kind: yaml.MappingNode}
	for index := range outcomes {
		next.Content = append(next.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("outcome-%04d", index)},
			&yaml.Node{Kind: yaml.ScalarNode, Value: "n000"},
		)
	}
	nodes := &yaml.Node{Kind: yaml.MappingNode}
	for index := range nodeCount {
		node := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "next"}, next,
		}}
		nodes.Content = append(nodes.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("n%03d", index)}, node,
		)
	}
	root := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "nodes"}, nodes,
		},
	}}}
	return root, next
}

func rawGraphDocumentWithScalarNext(nodeCount int) *yaml.Node {
	nodes := &yaml.Node{Kind: yaml.MappingNode}
	for index := range nodeCount {
		nodes.Content = append(nodes.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("n%04d", index)},
			&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: "next"},
				{Kind: yaml.ScalarNode, Value: "target"},
			}},
		)
	}
	return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "nodes"}, nodes,
		},
	}}}
}

func installUnsafeStructuralKey(root, mapping *yaml.Node, position, kind string) {
	index := 0
	if position == "after" {
		index = len(mapping.Content) - 2
	}
	if kind == "cycle" {
		cycle := &yaml.Node{Kind: yaml.AliasNode}
		cycle.Alias = cycle
		mapping.Content[index] = cycle
		return
	}
	node := &yaml.Node{Kind: yaml.ScalarNode, Value: "leaf"}
	for range yamlTreeNodeCount(root) + 1 {
		node = &yaml.Node{Kind: yaml.AliasNode, Alias: node}
	}
	mapping.Content[index] = node
}

func installUnsafeScalarNext(root *yaml.Node, kind string) {
	nodes := root.Content[0].Content[1]
	unsafe := &yaml.Node{Kind: yaml.AliasNode}
	if kind == "cycle" {
		unsafe.Alias = unsafe
	} else {
		leaf := &yaml.Node{Kind: yaml.ScalarNode, Value: "target"}
		unsafe = leaf
		// The enclosing node/key/value adds four source-tree nodes after this
		// chain is built; stay beyond that finite traversal allowance.
		for range yamlTreeNodeCount(root) + 8 {
			unsafe = &yaml.Node{Kind: yaml.AliasNode, Alias: unsafe}
		}
	}
	nodes.Content = append(nodes.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "scalar-alias"},
		&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "next"}, unsafe,
		}},
	)
}

func maximumFanoutTemplate() *Template {
	nodes := make(map[string]Node, MaxNormalizedNodes)
	branches := make(Next, MaxNormalizedDegree)
	for index := 0; index < MaxNormalizedDegree; index++ {
		branchID := fmt.Sprintf("branch-%04d", index)
		branches[branchID] = branchID
		nodes[branchID] = Node{
			Type:      NodeTypeTask,
			Performer: &Performer{Kind: PerformerAgent, Prompt: "work"},
			Next:      Next{"pass": "join"},
		}
	}
	nodes["fork"] = Node{Type: NodeTypeParallel, Next: branches}
	nodes["join"] = Node{Type: NodeTypeEnd, Join: JoinAll}
	return &Template{APIVersion: APIVersion, Kind: Kind, ID: "maximum", Start: "fork", Nodes: nodes}
}

func templateWithNodeCount(count int) *Template {
	nodes := make(map[string]Node, count)
	for index := 0; index < count; index++ {
		nodes[fmt.Sprintf("node-%04d", index)] = Node{}
	}
	return &Template{Start: "node-0000", Nodes: nodes}
}

func templateWithEdgeCount(count int) *Template {
	// The synthetic start edge consumes one. Spread authored edges across
	// sources so no individual map needs more than MaxNormalizedDegree keys.
	remaining := count - 1
	nodes := map[string]Node{}
	for sourceIndex := 0; remaining > 0; sourceIndex++ {
		degree := min(remaining, MaxNormalizedDegree)
		next := make(Next, degree)
		for edgeIndex := 0; edgeIndex < degree; edgeIndex++ {
			next[fmt.Sprintf("edge-%04d", edgeIndex)] = "target"
		}
		nodes[fmt.Sprintf("source-%d", sourceIndex)] = Node{Next: next}
		remaining -= degree
	}
	nodes["target"] = Node{}
	return &Template{Start: "target", Nodes: nodes}
}

func templateOverBothCardinalityLimits() *Template {
	tmpl := templateWithNodeCount(MaxNormalizedNodes + 1)
	node := tmpl.Nodes["node-0000"]
	node.Next = make(Next, MaxNormalizedEdges)
	for index := range MaxNormalizedEdges {
		node.Next[fmt.Sprintf("edge-%04d", index)] = "node-0000"
	}
	tmpl.Nodes["node-0000"] = node
	return tmpl
}

func semanticDiagnosticFloodTemplate(references int) *Template {
	return &Template{
		APIVersion: APIVersion,
		Kind:       Kind,
		ID:         "semantic-diagnostic-flood",
		Start:      "source",
		Nodes: map[string]Node{
			"source": {
				Type: NodeTypeTask,
				Performer: &Performer{
					Kind:   PerformerAgent,
					Prompt: strings.Repeat("{{ params.missing }}", references),
				},
				Next: Next{"pass": "target"},
			},
			"target": {Type: NodeTypeEnd},
		},
	}
}

func aliasedNextTemplateYAML(nodeCount, outcomes int, invalidBranch bool) []byte {
	var source strings.Builder
	source.WriteString("apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: aliases\nstart: n000\nnodes:\n")
	if invalidBranch {
		source.WriteString("  invalid: [not, a, node]\n")
	}
	for nodeIndex := 0; nodeIndex < nodeCount; nodeIndex++ {
		fmt.Fprintf(&source, "  n%03d:\n    type: end\n    next: ", nodeIndex)
		if nodeIndex == 0 {
			source.WriteString("&shared\n")
			for outcome := 0; outcome < outcomes; outcome++ {
				fmt.Fprintf(&source, "      outcome-%03d: n000\n", outcome)
			}
		} else {
			source.WriteString("*shared\n")
		}
	}
	return []byte(source.String())
}

func mixedScalarNextTemplateYAML(mappingNodes, outcomes, scalarNodes int) []byte {
	var source strings.Builder
	source.WriteString("apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: mixed-scalar\nstart: null\nnodes:\n")
	for nodeIndex := 0; nodeIndex < mappingNodes; nodeIndex++ {
		fmt.Fprintf(&source, "  map-%03d:\n    type: decision\n    next: ", nodeIndex)
		if nodeIndex == 0 {
			source.WriteString("&shared\n")
			for outcome := 0; outcome < outcomes; outcome++ {
				fmt.Fprintf(&source, "      outcome-%03d: target\n", outcome)
			}
		} else {
			source.WriteString("*shared\n")
		}
	}
	for nodeIndex := 0; nodeIndex < scalarNodes; nodeIndex++ {
		fmt.Fprintf(&source, "  scalar-%03d: {type: decision, next: target}\n", nodeIndex)
	}
	source.WriteString("  target: {type: end}\n")
	return []byte(source.String())
}

func replaceNodeKeyWithComplex(source string, nodeIndex int) string {
	return strings.Replace(source,
		fmt.Sprintf("  n%03d:\n", nodeIndex),
		"  ? [malformed-node-key]\n  :\n",
		1,
	)
}

func replaceOutcomeKeyWithComplex(source string, outcomeIndex int) string {
	return strings.Replace(source,
		fmt.Sprintf("      outcome-%03d: n000\n", outcomeIndex),
		"      ? [malformed-outcome-key]\n      : n000\n",
		1,
	)
}

func schemaAliasedNodeTemplateYAML(nodeCount, checks, unknownFields int) []byte {
	var source strings.Builder
	source.WriteString("apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: schema-aliases\nstart: n000\nnodes:\n")
	for nodeIndex := range nodeCount {
		fmt.Fprintf(&source, "  n%03d: ", nodeIndex)
		if nodeIndex != 0 {
			source.WriteString("*shared\n")
			continue
		}
		source.WriteString("&shared\n    type: task\n    performer:\n      kind: agent\n      prompt: work\n    checks:\n")
		for check := range checks {
			fmt.Fprintf(&source, "      - id: check-%03d\n        performer:\n          kind: program\n          run: echo\n", check)
			for unknown := range unknownFields {
				fmt.Fprintf(&source, "        unknown-%03d: value\n", unknown)
			}
		}
	}
	return []byte(source.String())
}

func duplicateMetadataTemplateYAML(duplicates int, appendSchemaFlood bool) []byte {
	var source strings.Builder
	source.WriteString("apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: duplicate-budget\nstart: n000\nnodes:\n  n000:\n    type: task\n    performer: {kind: agent, prompt: work}\n    metadata:\n")
	for range duplicates {
		source.WriteString("      repeated: value\n")
	}
	if appendSchemaFlood {
		source.WriteString("    checks: &checks\n")
		for check := range MaxTemplateAuthoringDiagnostics {
			fmt.Fprintf(&source, "      - id: check-%04d\n        performer: {kind: program, run: echo}\n        unknown-000: value\n", check)
		}
	}
	return []byte(source.String())
}

func countDiagnosticCode(diagnostics Diagnostics, code string) int {
	count := 0
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			count++
		}
	}
	return count
}

func diagnosticCodes(diagnostics Diagnostics) []string {
	codes := make([]string, len(diagnostics))
	for index, diagnostic := range diagnostics {
		codes[index] = diagnostic.Code
	}
	return codes
}
