package model

import (
	"errors"
	"fmt"
)

const (
	// MaxNormalizedDegree is the authoring ceiling derived from the path-v1
	// aggregate mutation budget. Execution reducers recheck their own tighter
	// capability-specific bounds before append.
	MaxNormalizedDegree = 2_046

	// MaxNormalizedNodes admits a complete maximum-width fork: the existing
	// 2,046 branch ceiling plus the fork and its structural reducer.
	MaxNormalizedNodes = MaxNormalizedDegree + 2

	// MaxNormalizedEdges admits both legs of that maximum-width fork while
	// staying aligned with the 4,096-entry routing/viewer operational scale.
	MaxNormalizedEdges = 2 * MaxNormalizedNodes

	DiagnosticCodeNormalizedNodeLimit = "normalized_node_limit"
	DiagnosticCodeNormalizedEdgeLimit = "normalized_edge_limit"
	DiagnosticCodeGraphAliasLimit     = "normalized_graph_alias_limit"
)

// NormalizedGraphCardinality is the bounded count used before and after edge
// normalization. Values stop at maximum+1: diagnostics need to distinguish
// accepted from rejected input, not disclose or keep walking hostile sizes.
type NormalizedGraphCardinality struct {
	Nodes int
	Edges int
}

// ErrNormalizedGraphBudget classifies direct canonicalization attempts that
// exceed the normalized node or edge budget.
var ErrNormalizedGraphBudget = errors.New("process template exceeds normalized graph budget")

// NormalizedGraphBudgetError prevents direct canonicalization/hash callers
// from bypassing the validation boundary and performing graph-wide work on an
// over-budget materialized template.
type NormalizedGraphBudgetError struct {
	Diagnostics Diagnostics
}

func (e *NormalizedGraphBudgetError) Error() string {
	if e == nil || len(e.Diagnostics) == 0 {
		return ErrNormalizedGraphBudget.Error()
	}
	return e.Diagnostics[0].Code + ": " + e.Diagnostics[0].Message
}

func (e *NormalizedGraphBudgetError) Unwrap() error { return ErrNormalizedGraphBudget }

func requireNormalizedGraphBudget(tmpl *Template) error {
	diagnostics := PreflightNormalizedGraphCardinality(tmpl)
	if !diagnostics.HasErrors() {
		return nil
	}
	return &NormalizedGraphBudgetError{Diagnostics: diagnostics}
}

func graphAliasLimitDiagnostic() Diagnostic {
	return diagError(
		DiagnosticCodeGraphAliasLimit,
		"nodes",
		"process template graph aliases exceed the parsed YAML structural-resolution budget",
	)
}

// PreflightNormalizedGraphCardinality counts the graph represented by a
// materialized Template without allocating its normalized edge slice. A
// non-empty start contributes the one synthetic start edge.
func PreflightNormalizedGraphCardinality(tmpl *Template) Diagnostics {
	if tmpl == nil {
		return nil
	}
	counts := NormalizedGraphCardinality{
		Nodes: saturatingCount(len(tmpl.Nodes), MaxNormalizedNodes),
	}
	if tmpl.Start != "" {
		counts.Edges = 1
	}
	for _, node := range tmpl.Nodes {
		counts.Edges = saturatingAdd(counts.Edges, len(node.Next), MaxNormalizedEdges)
		if counts.Edges > MaxNormalizedEdges {
			break
		}
	}
	return normalizedGraphCardinalityDiagnostics(counts)
}

// NormalizeEdgesWithinBudget applies the allocation preflight before building
// normalized edges. Validate remains the authoritative post-normalization
// check; this helper prevents a compact or direct input from allocating work
// that is already provably outside the same budget.
func NormalizeEdgesWithinBudget(tmpl *Template) ([]Edge, Diagnostics) {
	if tmpl == nil {
		return nil, nil
	}
	if diagnostics := PreflightNormalizedGraphCardinality(tmpl); diagnostics.HasErrors() {
		return nil, diagnostics
	}
	edges := NormalizeEdges(tmpl)
	return edges, normalizedGraphCardinalityDiagnostics(NormalizedGraphCardinality{
		Nodes: saturatingCount(len(tmpl.Nodes), MaxNormalizedNodes),
		Edges: saturatingCount(len(edges), MaxNormalizedEdges),
	})
}

func normalizedGraphCardinalityDiagnostics(counts NormalizedGraphCardinality) Diagnostics {
	var diagnostics Diagnostics
	if counts.Nodes > MaxNormalizedNodes {
		diagnostics = append(diagnostics, diagError(
			DiagnosticCodeNormalizedNodeLimit,
			"nodes",
			fmt.Sprintf("normalized node count exceeds %d (counted at least %d)", MaxNormalizedNodes, MaxNormalizedNodes+1),
		))
	}
	if counts.Edges > MaxNormalizedEdges {
		diagnostics = append(diagnostics, diagError(
			DiagnosticCodeNormalizedEdgeLimit,
			"nodes",
			fmt.Sprintf("normalized edge count exceeds %d (counted at least %d, including the synthetic start edge when present)", MaxNormalizedEdges, MaxNormalizedEdges+1),
		))
	}
	return diagnostics
}

func saturatingCount(value, maximum int) int {
	if value > maximum {
		return maximum + 1
	}
	return value
}

func saturatingAdd(current, increment, maximum int) int {
	if current > maximum || increment > maximum-current {
		return maximum + 1
	}
	return current + increment
}
