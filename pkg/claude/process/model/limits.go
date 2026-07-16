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

	// Authoring findings are bounded at the same aggregate graph-work scale.
	// Wire accounting reserves a small response envelope inside the same 4 MiB
	// public source/request scale, then charges worst-case JSON expansion and a
	// possible second copy of the path as an editor target.
	MaxProcessTemplateSourceBytes   = 4 << 20
	MaxTemplateAuthoringDiagnostics = MaxNormalizedNodes + MaxNormalizedEdges
	MaxTemplateDiagnosticWireBytes  = MaxProcessTemplateSourceBytes - (4 << 10)

	templateDiagnosticJSONExpansion  = 6
	templateDiagnosticFixedWireBytes = 256

	DiagnosticCodeNormalizedNodeLimit = "normalized_node_limit"
	DiagnosticCodeNormalizedEdgeLimit = "normalized_edge_limit"
	DiagnosticCodeGraphAliasLimit     = "normalized_graph_alias_limit"
	DiagnosticCodeInvalidGraphKey     = "invalid_graph_key"
	DiagnosticCodeSchemaBudget        = "template_schema_budget"
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

// HasNormalizedGraphBudgetError reports resource-limit diagnostics that must
// reject persistence or graph-wide processing even where ordinary semantic
// validation findings remain editable draft warnings.
func (d Diagnostics) HasNormalizedGraphBudgetError() bool {
	for _, diagnostic := range d {
		switch diagnostic.Code {
		case DiagnosticCodeNormalizedNodeLimit,
			DiagnosticCodeNormalizedEdgeLimit,
			DiagnosticCodeGraphAliasLimit,
			DiagnosticCodeSchemaBudget:
			return true
		}
	}
	return false
}

func schemaBudgetDiagnostic() Diagnostic {
	return diagError(
		DiagnosticCodeSchemaBudget,
		"",
		fmt.Sprintf("process template source diagnostics exceed the bounded authoring budget (%d findings or %d encoded bytes)", MaxTemplateAuthoringDiagnostics, MaxTemplateDiagnosticWireBytes),
	)
}

type templateDiagnosticBudget struct {
	count     int
	wireBytes int
	exhausted bool
}

func (b *templateDiagnosticBudget) fits(codeBytes, pathBytes, messageBytes int) bool {
	if b == nil || b.exhausted || b.count >= MaxTemplateAuthoringDiagnostics-1 {
		if b != nil {
			b.exhausted = true
		}
		return false
	}
	cost := templateDiagnosticWireCost(codeBytes, pathBytes, messageBytes)
	sentinel := schemaBudgetDiagnostic()
	sentinelCost := templateDiagnosticWireCost(len(sentinel.Code), len(sentinel.Path), len(sentinel.Message))
	if cost > MaxTemplateDiagnosticWireBytes-sentinelCost-b.wireBytes {
		b.exhausted = true
		return false
	}
	return true
}

func (b *templateDiagnosticBudget) append(diagnostics *Diagnostics, diagnostic Diagnostic) bool {
	if !b.fits(len(diagnostic.Code), len(diagnostic.Path), len(diagnostic.Message)) {
		return false
	}
	*diagnostics = append(*diagnostics, diagnostic)
	b.count++
	b.wireBytes += templateDiagnosticWireCost(len(diagnostic.Code), len(diagnostic.Path), len(diagnostic.Message))
	return true
}

func templateDiagnosticWireCost(codeBytes, pathBytes, messageBytes int) int {
	maximum := MaxTemplateDiagnosticWireBytes + 1
	textBytes := saturatingAdd(codeBytes, saturatingAdd(pathBytes, pathBytes, maximum), maximum)
	textBytes = saturatingAdd(textBytes, messageBytes, maximum)
	expanded := maximum
	if textBytes <= maximum/templateDiagnosticJSONExpansion {
		expanded = textBytes * templateDiagnosticJSONExpansion
	}
	return saturatingAdd(templateDiagnosticFixedWireBytes, expanded, maximum)
}

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

func invalidGraphKeyDiagnostic(path string) Diagnostic {
	return diagError(
		DiagnosticCodeInvalidGraphKey,
		path,
		"process template graph mapping keys must decode to strings",
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
