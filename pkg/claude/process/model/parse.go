package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

// Parse is the public editable-authoring-source parser. It retains the
// historical name for CLI/editor callers and may perform explicitly documented
// source migrations such as metadata.join promotion. Pinned/execution source
// must use ParseExactSource instead.
func Parse(data []byte) (*ParsedTemplate, error) {
	return ParseAuthoring(data)
}

// ParseAuthoring parses editable YAML and applies authoring-only promotions.
func ParseAuthoring(data []byte) (*ParsedTemplate, error) {
	return parseSource(data, true)
}

// ParseExactSource parses the YAML source paired with an immutable template
// version. It validates and hashes exactly the modeled fields present and
// never promotes advisory legacy metadata.
func ParseExactSource(data []byte) (*ParsedTemplate, error) {
	return parseSource(data, false)
}

func parseSource(data []byte, authoring bool) (*ParsedTemplate, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse process template YAML: %w", err)
	}

	diagnosticBudget := &templateDiagnosticBudget{}
	diagnostics := duplicateKeyDiagnostics(&root, diagnosticBudget)
	if diagnosticBudget.exhausted {
		return &ParsedTemplate{
			Diagnostics: append(diagnostics, schemaBudgetDiagnostic()),
			SourceHash:  hashBytes(data),
		}, nil
	}
	pruneDuplicateKeys(&root)
	cardinality, cardinalityStatus, structuralDiagnostics := rawNormalizedGraphCardinality(&root)
	if cardinalityDiagnostics := normalizedGraphCardinalityDiagnostics(cardinality); cardinalityDiagnostics.HasErrors() {
		for _, diagnostic := range cardinalityDiagnostics {
			if !diagnosticBudget.append(&diagnostics, diagnostic) {
				diagnostics = append(diagnostics, schemaBudgetDiagnostic())
				break
			}
		}
		return &ParsedTemplate{
			Diagnostics: diagnostics,
			SourceHash:  hashBytes(data),
		}, nil
	}
	if cardinalityStatus == rawGraphAliasUnsafe {
		if !diagnosticBudget.append(&diagnostics, graphAliasLimitDiagnostic()) {
			diagnostics = append(diagnostics, schemaBudgetDiagnostic())
		}
		return &ParsedTemplate{Diagnostics: diagnostics, SourceHash: hashBytes(data)}, nil
	}
	if cardinalityStatus == rawGraphRejected {
		for _, diagnostic := range structuralDiagnostics {
			if !diagnosticBudget.append(&diagnostics, diagnostic) {
				diagnostics = append(diagnostics, schemaBudgetDiagnostic())
				break
			}
		}
		return &ParsedTemplate{
			Diagnostics: diagnostics,
			SourceHash:  hashBytes(data),
		}, nil
	}
	// Schema traversal resolves aliases recursively. Keep it behind the
	// saturating graph walk so repeated references cannot amplify diagnostic
	// work before the normalized-cardinality allocation guard has run.
	schemaDiagnostics := unknownFieldDiagnostics(&root, diagnosticBudget)
	diagnostics = append(diagnostics, schemaDiagnostics...)
	if schemaDiagnostics.HasNormalizedGraphBudgetError() {
		return &ParsedTemplate{Diagnostics: diagnostics, SourceHash: hashBytes(data)}, nil
	}

	var tmpl Template
	if err := root.Decode(&tmpl); err != nil {
		return nil, fmt.Errorf("decode process template: %w", err)
	}

	normalizeTemplate(&tmpl)
	diagnostics = append(diagnostics, normalizeFreeform(&tmpl)...)
	if authoring {
		diagnostics = append(diagnostics, promoteLegacyJoins(&tmpl)...)
	}
	edges, cardinalityDiagnostics := NormalizeEdgesWithinBudget(&tmpl)
	diagnostics = append(diagnostics, cardinalityDiagnostics...)
	if cardinalityDiagnostics.HasErrors() {
		return &ParsedTemplate{
			Template: &tmpl, Edges: edges, Diagnostics: diagnostics,
			SourceHash: hashBytes(data),
		}, nil
	}
	diagnostics = append(diagnostics, Validate(&tmpl, edges)...)

	semanticHash, err := SemanticHash(&tmpl)
	if err != nil {
		return nil, err
	}
	sourceHash := hashBytes(data)

	parsed := &ParsedTemplate{
		Template:     &tmpl,
		Edges:        edges,
		Diagnostics:  diagnostics,
		SemanticHash: semanticHash,
		SourceHash:   sourceHash,
		Ref:          TemplateRef(tmpl.ID, semanticHash),
	}
	return parsed, nil
}

// rawNormalizedGraphCardinality inspects only the structural graph fields of
// the already-pruned YAML tree. It runs before Decode so a compact anchored
// next map cannot be materialized once per referring node beyond the graph
// budget. Uncountable wrong-kind shapes defer to Decode; alias-resolution
// exhaustion and cycles fail closed instead of bypassing the allocation guard.
type rawGraphCardinalityStatus uint8

const (
	rawGraphUncountable rawGraphCardinalityStatus = iota
	rawGraphCounted
	rawGraphAliasUnsafe
	rawGraphRejected
)

type rawGraphStructuralIssue uint8

const (
	rawGraphNoStructuralIssue rawGraphStructuralIssue = iota
	rawGraphInvalidKey
	rawGraphUnsafeAlias
)

type rawNextCountResult struct {
	count int
	issue rawGraphStructuralIssue
}

func rawNormalizedGraphCardinality(root *yaml.Node) (NormalizedGraphCardinality, rawGraphCardinalityStatus, Diagnostics) {
	aliasSteps := yamlTreeNodeCount(root)
	var structuralDiagnostics Diagnostics
	recordStructuralDiagnostic := func(diagnostic Diagnostic) {
		if len(structuralDiagnostics) == 0 {
			structuralDiagnostics = Diagnostics{diagnostic}
		}
	}
	recordInvalidGraphKey := func(path string) {
		recordStructuralDiagnostic(invalidGraphKeyDiagnostic(path))
	}
	recordStructuralIssue := func(issue rawGraphStructuralIssue, path string) {
		switch issue {
		case rawGraphInvalidKey:
			recordInvalidGraphKey(path)
		case rawGraphUnsafeAlias:
			recordStructuralDiagnostic(graphAliasLimitDiagnosticAt(path))
		}
	}
	root, status := structuralNode(root, aliasSteps)
	if status != rawGraphCounted {
		return NormalizedGraphCardinality{}, rawGraphRejected, Diagnostics{graphAliasLimitDiagnostic()}
	}
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) != 1 {
			return NormalizedGraphCardinality{}, rawGraphUncountable, nil
		}
		root, status = structuralNode(root.Content[0], aliasSteps)
	}
	if status != rawGraphCounted {
		return NormalizedGraphCardinality{}, rawGraphRejected, Diagnostics{graphAliasLimitDiagnostic()}
	}
	if root.Kind != yaml.MappingNode {
		return NormalizedGraphCardinality{}, rawGraphUncountable, nil
	}
	if key := mappingMergeKey(root, aliasSteps); key != nil {
		recordStructuralDiagnostic(mergeKeyDiag(key, ""))
	}

	counts := NormalizedGraphCardinality{}
	start, rootIssue := mappingValue(root, "start", aliasSteps)
	recordStructuralIssue(rootIssue, "")
	if start != nil {
		startValue, startStatus := structuralMappingKey(start, aliasSteps)
		if startStatus == rawGraphAliasUnsafe {
			recordStructuralIssue(rawGraphUnsafeAlias, "start")
		}
		if startStatus == rawGraphCounted && startValue != "" {
			counts.Edges = 1
		}
	}
	nodes, rootIssue := mappingValue(root, "nodes", aliasSteps)
	recordStructuralIssue(rootIssue, "")
	if nodes == nil {
		if len(structuralDiagnostics) > 0 {
			return counts, rawGraphRejected, structuralDiagnostics
		}
		return counts, rawGraphCounted, nil
	}
	nodes, status = structuralNode(nodes, aliasSteps)
	if status != rawGraphCounted {
		recordStructuralIssue(rawGraphUnsafeAlias, "nodes")
		return counts, rawGraphRejected, structuralDiagnostics
	}
	if nodes.Kind != yaml.MappingNode {
		return counts, rawGraphUncountable, structuralDiagnostics
	}
	if key := mappingMergeKey(nodes, aliasSteps); key != nil {
		recordStructuralDiagnostic(mergeKeyDiag(key, "nodes"))
	}
	seenNodes := make(map[string]struct{}, min(len(nodes.Content)/2, MaxNormalizedNodes+1))
	nextCounts := make(map[*yaml.Node]rawNextCountResult, min(len(nodes.Content)/2, MaxNormalizedNodes+1))
	for index := len(nodes.Content) - 2; index >= 0; index -= 2 {
		// Every surviving mapping entry consumes a normalized-node slot even
		// when its key is malformed. Key decoding determines identity and the
		// diagnostic, never whether hostile graph work is charged.
		counts.Nodes = saturatingAdd(counts.Nodes, 1, MaxNormalizedNodes)
		nodeID, nodeKeyStatus := structuralMappingKey(nodes.Content[index], aliasSteps)
		nodePath := "nodes"
		if nodeKeyStatus == rawGraphCounted {
			if _, duplicate := seenNodes[nodeID]; duplicate {
				continue
			}
			seenNodes[nodeID] = struct{}{}
			nodePath = joinPath("nodes", nodeID)
		} else if nodeKeyStatus == rawGraphAliasUnsafe {
			recordStructuralIssue(rawGraphUnsafeAlias, "nodes")
		} else {
			recordStructuralIssue(rawGraphInvalidKey, "nodes")
		}
		if counts.Edges > MaxNormalizedEdges {
			continue
		}
		node, nodeStatus := structuralNode(nodes.Content[index+1], aliasSteps)
		if nodeStatus != rawGraphCounted {
			recordStructuralIssue(rawGraphUnsafeAlias, nodePath)
			continue
		}
		if node.Kind != yaml.MappingNode {
			continue
		}
		if key := mappingMergeKey(node, aliasSteps); key != nil {
			recordStructuralDiagnostic(mergeKeyDiag(key, nodePath))
		}
		next, nodeIssue := mappingValue(node, "next", aliasSteps)
		recordStructuralIssue(nodeIssue, nodePath)
		if next == nil {
			continue
		}
		next, nextStatus := structuralNode(next, aliasSteps)
		if nextStatus != rawGraphCounted {
			recordStructuralIssue(rawGraphUnsafeAlias, joinPath(nodePath, "next"))
			continue
		}
		if next.Kind != yaml.MappingNode {
			continue
		}
		result, cached := nextCounts[next]
		if !cached {
			result = rawNextEdgeCount(next, aliasSteps)
			nextCounts[next] = result
		}
		// The cached issue is relative to the shared next map. Bind it to the
		// authoritative occurrence being traversed, never to the anchor site.
		recordStructuralIssue(result.issue, joinPath(nodePath, "next"))
		// A cached anchored map still contributes once per referring node;
		// caching bounds traversal without memoizing away alias multiplicity.
		counts.Edges = saturatingAdd(counts.Edges, result.count, MaxNormalizedEdges)
		if counts.Nodes > MaxNormalizedNodes && counts.Edges > MaxNormalizedEdges {
			break
		}
	}
	if len(structuralDiagnostics) > 0 {
		return counts, rawGraphRejected, structuralDiagnostics
	}
	return counts, rawGraphCounted, nil
}

func rawNextEdgeCount(next *yaml.Node, maximumAliasSteps int) rawNextCountResult {
	seenOutcomes := make(map[string]struct{}, min(len(next.Content)/2, MaxNormalizedEdges+1))
	count := 0
	issue := rawGraphNoStructuralIssue
	for edgeIndex := 0; edgeIndex+1 < len(next.Content); edgeIndex += 2 {
		edgeKey := next.Content[edgeIndex]
		// Resolve aliases with the finite structural walker before consulting
		// yaml.Node tags: ShortTag recursively follows aliases on its own and
		// would overflow the Go stack on a cyclic programmatic node graph.
		resolvedKey, keyStatus := structuralNode(edgeKey, maximumAliasSteps)
		if keyStatus == rawGraphCounted && resolvedKey.ShortTag() == mergeTag {
			// Next.UnmarshalYAML deliberately skips merge entries after the raw
			// schema emits merge_key_unsupported, so they contribute no edge.
			continue
		}
		// Charge the entry before decoding its key. Malformed keys still make
		// yaml.v3 attempt a map insertion, so they cannot hide alias-amplified
		// work from the allocation guard.
		count = saturatingAdd(count, 1, MaxNormalizedEdges)
		if keyStatus != rawGraphCounted {
			if issue == rawGraphNoStructuralIssue {
				issue = rawGraphUnsafeAlias
			}
			if count > MaxNormalizedEdges {
				break
			}
			continue
		}
		outcome, ok := decodedStructuralScalar(resolvedKey)
		if !ok {
			if issue == rawGraphNoStructuralIssue {
				issue = rawGraphInvalidKey
			}
			if count > MaxNormalizedEdges {
				break
			}
			continue
		}
		if _, duplicate := seenOutcomes[outcome]; duplicate {
			count--
			continue
		}
		seenOutcomes[outcome] = struct{}{}
		if count > MaxNormalizedEdges {
			break
		}
	}
	return rawNextCountResult{count: count, issue: issue}
}

func mappingMergeKey(mapping *yaml.Node, maximumAliasSteps int) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		resolved, status := structuralNode(mapping.Content[index], maximumAliasSteps)
		if status == rawGraphCounted && resolved.ShortTag() == mergeTag {
			return mapping.Content[index]
		}
	}
	return nil
}

func mappingValue(mapping *yaml.Node, key string, maximumAliasSteps int) (*yaml.Node, rawGraphStructuralIssue) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, rawGraphNoStructuralIssue
	}
	var found *yaml.Node
	issue := rawGraphNoStructuralIssue
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		resolved, status := structuralMappingKey(mapping.Content[index], maximumAliasSteps)
		if status == rawGraphAliasUnsafe {
			if issue == rawGraphNoStructuralIssue {
				issue = rawGraphUnsafeAlias
			}
			continue
		}
		if status != rawGraphCounted {
			if issue == rawGraphNoStructuralIssue {
				issue = rawGraphInvalidKey
			}
			continue
		}
		if resolved == key {
			found = mapping.Content[index+1]
		}
	}
	return found, issue
}

func structuralMappingKey(key *yaml.Node, maximumAliasSteps int) (string, rawGraphCardinalityStatus) {
	key, status := structuralNode(key, maximumAliasSteps)
	if status != rawGraphCounted {
		return "", status
	}
	value, ok := decodedStructuralScalar(key)
	if !ok {
		return "", rawGraphUncountable
	}
	return value, rawGraphCounted
}

func structuralNode(node *yaml.Node, maximumSteps int) (*yaml.Node, rawGraphCardinalityStatus) {
	if node == nil {
		return nil, rawGraphUncountable
	}
	if node.Kind != yaml.AliasNode {
		return node, rawGraphCounted
	}
	seen := make(map[*yaml.Node]struct{})
	for steps := 0; node != nil && node.Kind == yaml.AliasNode; steps++ {
		if steps >= maximumSteps {
			return nil, rawGraphAliasUnsafe
		}
		if _, duplicate := seen[node]; duplicate {
			return nil, rawGraphAliasUnsafe
		}
		seen[node] = struct{}{}
		node = node.Alias
	}
	if node == nil {
		return nil, rawGraphUncountable
	}
	return node, rawGraphCounted
}

func yamlTreeNodeCount(root *yaml.Node) int {
	if root == nil {
		return 0
	}
	count := 0
	stack := []*yaml.Node{root}
	for len(stack) > 0 {
		last := len(stack) - 1
		node := stack[last]
		stack = stack[:last]
		if node == nil {
			continue
		}
		count++
		stack = append(stack, node.Content...)
	}
	return count
}

// promoteLegacyJoins is deliberately confined to Parse, the authoring-source
// boundary. Immutable template.json reads decode Template directly and must
// never reinterpret advisory metadata under an already-pinned semantic hash.
func promoteLegacyJoins(tmpl *Template) Diagnostics {
	if tmpl == nil {
		return nil
	}
	var diagnostics Diagnostics
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		node := tmpl.Nodes[nodeID]
		legacy, present := node.Metadata["join"]
		if !present {
			continue
		}
		value, ok := legacy.(string)
		policy := JoinPolicy(value)
		if !ok || policy != JoinAll && policy != JoinAny {
			diagnostics = append(diagnostics, diagError("invalid_legacy_join", "nodes."+nodeID+".metadata.join", "legacy metadata.join must be all or any"))
			continue
		}
		if node.Join == "" {
			node.Join = policy
		} else if node.Join != policy {
			diagnostics = append(diagnostics, diagError("join_metadata_conflict", "nodes."+nodeID+".metadata.join",
				fmt.Sprintf("typed join %q disagrees with legacy metadata.join %q", node.Join, policy)))
		}
		delete(node.Metadata, "join")
		tmpl.Nodes[nodeID] = node
	}
	return diagnostics
}

// mergeTag is the resolved short tag yaml.v3 assigns to a `<<` merge key.
const mergeTag = "!!merge"

// mappingKeyID identifies a mapping key for duplicate detection and pruning.
//
// The process model is string-keyed: schema maps decode into map[string]... and
// freeform maps normalize non-string scalar keys to their string form. So
// scalars that render to the same string — e.g. `1` (!!int) and `"1"` (!!str) —
// deliberately collide here and resolve last-wins. Distinguishing them by tag
// would let both survive pruning and then hard-fail Decode on the string-keyed
// target, replacing a clean duplicate_key diagnostic with a raw YAML error, so
// we key on the scalar value alone. Non-scalar (complex) keys have an empty
// Value; they are exotic, unsupported by the model, and rejected by Decode
// regardless.
func mappingKeyID(node *yaml.Node) string {
	if node == nil {
		return ""
	}
	if value, ok := decodedStructuralScalar(node); ok {
		return value
	}
	return node.Value
}

// decodedStructuralScalar mirrors yaml.v3's Decode-to-string semantics for
// Template.Start and string-keyed graph maps. It follows scalar aliases with
// cycle protection; complex/wrong-kind keys remain the decoder's error.
func decodedStructuralScalar(node *yaml.Node) (string, bool) {
	if node == nil {
		return "", false
	}
	if node.Kind != yaml.AliasNode {
		if node.Kind != yaml.ScalarNode {
			return "", false
		}
		var value string
		if err := node.Decode(&value); err != nil {
			return "", false
		}
		return value, true
	}
	seen := make(map[*yaml.Node]struct{})
	for node != nil && node.Kind == yaml.AliasNode {
		if _, cycle := seen[node]; cycle {
			return "", false
		}
		seen[node] = struct{}{}
		node = node.Alias
	}
	if node == nil || node.Kind != yaml.ScalarNode {
		return "", false
	}
	var value string
	if err := node.Decode(&value); err != nil {
		return "", false
	}
	return value, true
}

func pruneDuplicateKeys(root *yaml.Node) {
	var walk func(node *yaml.Node)
	walk = func(node *yaml.Node) {
		if node == nil {
			return
		}
		if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
			walk(node.Content[0])
			return
		}
		if node.Kind == yaml.MappingNode {
			// YAML convention is last-wins for duplicate keys; keep each key at
			// its final occurrence so the decoded template (and its semantic
			// hash) match standard YAML semantics.
			lastIndex := map[string]int{}
			for i := 0; i < len(node.Content); i += 2 {
				lastIndex[mappingKeyID(node.Content[i])] = i
			}
			pruned := make([]*yaml.Node, 0, len(node.Content))
			for i := 0; i < len(node.Content); i += 2 {
				if lastIndex[mappingKeyID(node.Content[i])] != i {
					continue
				}
				pruned = append(pruned, node.Content[i], node.Content[i+1])
				walk(node.Content[i+1])
			}
			node.Content = pruned
			return
		}
		if node.Kind == yaml.SequenceNode {
			for _, child := range node.Content {
				walk(child)
			}
		}
	}
	walk(root)
}

// NormalizeEdges is the low-level deterministic projection. Production entry
// points must call NormalizeEdgesWithinBudget so compact/direct input is
// rejected before allocating this slice.
func NormalizeEdges(tmpl *Template) []Edge {
	if tmpl == nil {
		return nil
	}

	var edges []Edge
	if tmpl.Start != "" {
		edges = append(edges, Edge{From: "", Outcome: "start", To: tmpl.Start})
	}

	nodeIDs := sortedKeys(tmpl.Nodes)
	for _, nodeID := range nodeIDs {
		next := tmpl.Nodes[nodeID].Next
		outcomes := sortedKeys(next)
		for _, outcome := range outcomes {
			edges = append(edges, Edge{From: nodeID, Outcome: outcome, To: next[outcome]})
		}
	}
	return edges
}

func SemanticHash(tmpl *Template) (string, error) {
	data, err := CanonicalSemanticJSON(tmpl)
	if err != nil {
		return "", err
	}
	return hashBytes(data), nil
}

func TemplateRef(id, semanticHash string) string {
	if id == "" || semanticHash == "" {
		return ""
	}
	return id + "@" + HashAlgorithm + ":" + semanticHash
}

func CanonicalSemanticJSON(tmpl *Template) ([]byte, error) {
	if tmpl == nil {
		return nil, fmt.Errorf("nil process template")
	}
	if err := requireNormalizedGraphBudget(tmpl); err != nil {
		return nil, err
	}

	semantic := cloneTemplate(tmpl)
	semantic.Layout = nil
	normalizeTemplate(&semantic)

	data, err := json.Marshal(semantic)
	if err != nil {
		return nil, fmt.Errorf("canonicalize process template semantics: %w", err)
	}
	return data, nil
}

func CanonicalYAML(tmpl *Template) ([]byte, error) {
	if tmpl == nil {
		return nil, fmt.Errorf("nil process template")
	}
	if err := requireNormalizedGraphBudget(tmpl); err != nil {
		return nil, err
	}
	clone := cloneTemplate(tmpl)
	normalizeTemplate(&clone)
	data, err := yaml.Marshal(&clone)
	if err != nil {
		return nil, fmt.Errorf("canonicalize process template YAML: %w", err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	return data, nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func normalizeTemplate(tmpl *Template) {
	if tmpl.Params == nil {
		tmpl.Params = map[string]Param{}
	}
	if tmpl.Nodes == nil {
		tmpl.Nodes = map[string]Node{}
	}
	for id, node := range tmpl.Nodes {
		if node.Metadata == nil {
			node.Metadata = Metadata{}
		}
		tmpl.Nodes[id] = node
	}
}

func cloneTemplate(tmpl *Template) Template {
	clone := *tmpl
	if tmpl.Params != nil {
		clone.Params = make(map[string]Param, len(tmpl.Params))
		for key, value := range tmpl.Params {
			clone.Params[key] = value
		}
	}
	if tmpl.Nodes != nil {
		clone.Nodes = make(map[string]Node, len(tmpl.Nodes))
		for key, value := range tmpl.Nodes {
			value.Checks = append([]Step(nil), value.Checks...)
			if value.Next != nil {
				next := make(Next, len(value.Next))
				for outcome, target := range value.Next {
					next[outcome] = target
				}
				value.Next = next
			}
			if value.Metadata != nil {
				metadata := make(Metadata, len(value.Metadata))
				for metadataKey, metadataValue := range value.Metadata {
					metadata[metadataKey] = metadataValue
				}
				value.Metadata = metadata
			}
			clone.Nodes[key] = value
		}
	}
	if tmpl.Layout != nil {
		layout := *tmpl.Layout
		if tmpl.Layout.Nodes != nil {
			layout.Nodes = make(map[string]LayoutNode, len(tmpl.Layout.Nodes))
			for key, value := range tmpl.Layout.Nodes {
				layout.Nodes[key] = value
			}
		}
		clone.Layout = &layout
	}
	return clone
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func sortAnyValues(values []any) {
	slices.SortFunc(values, func(a, b any) int {
		typeA := fmt.Sprintf("%T", a)
		typeB := fmt.Sprintf("%T", b)
		if typeA < typeB {
			return -1
		}
		if typeA > typeB {
			return 1
		}
		valueA := fmt.Sprint(a)
		valueB := fmt.Sprint(b)
		if valueA < valueB {
			return -1
		}
		if valueA > valueB {
			return 1
		}
		return 0
	})
}

func duplicateKeyDiagnostics(root *yaml.Node, budget *templateDiagnosticBudget) Diagnostics {
	var diagnostics Diagnostics
	var walk func(node *yaml.Node, path string)
	walk = func(node *yaml.Node, path string) {
		if node == nil || budget.exhausted {
			return
		}
		if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
			walk(node.Content[0], path)
			return
		}
		if node.Kind == yaml.MappingNode {
			seen := map[string]struct{}{}
			for i := 0; i < len(node.Content); i += 2 {
				if budget.exhausted {
					break
				}
				key := node.Content[i]
				value := node.Content[i+1]
				id := mappingKeyID(key)
				keyPath := joinPath(path, id)
				if _, ok := seen[id]; ok {
					budget.append(&diagnostics, diagErrorAt("duplicate_key", keyPath, "duplicate YAML mapping key", key))
				}
				seen[id] = struct{}{}
				walk(value, keyPath)
			}
			return
		}
		if node.Kind == yaml.SequenceNode {
			for i, child := range node.Content {
				walk(child, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(root, "")
	return diagnostics
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

// withPos carries a yaml.Node's 1-based source position onto a diagnostic.
func withPos(d Diagnostic, node *yaml.Node) Diagnostic {
	if node != nil {
		d.Line = node.Line
		d.Col = node.Column
	}
	return d
}

func diagErrorAt(code, path, message string, node *yaml.Node) Diagnostic {
	return withPos(diagError(code, path, message), node)
}
