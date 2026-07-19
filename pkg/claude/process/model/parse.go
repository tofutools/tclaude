package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

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
			Diagnostics: append(diagnostics, templateDiagnosticBudgetDiagnostic()),
			SourceHash:  hashBytes(data),
		}, nil
	}
	root = *pruneDuplicateKeys(&root)
	cardinality, cardinalityStatus, structuralDiagnostics := rawNormalizedGraphCardinality(&root)
	if cardinalityDiagnostics := normalizedGraphCardinalityDiagnostics(cardinality); cardinalityDiagnostics.HasErrors() {
		for _, diagnostic := range cardinalityDiagnostics {
			if !diagnosticBudget.append(&diagnostics, diagnostic) {
				diagnostics = append(diagnostics, templateDiagnosticBudgetDiagnostic())
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
			diagnostics = append(diagnostics, templateDiagnosticBudgetDiagnostic())
		}
		return &ParsedTemplate{Diagnostics: diagnostics, SourceHash: hashBytes(data)}, nil
	}
	if cardinalityStatus == rawGraphRejected {
		for _, diagnostic := range structuralDiagnostics {
			if !diagnosticBudget.append(&diagnostics, diagnostic) {
				diagnostics = append(diagnostics, templateDiagnosticBudgetDiagnostic())
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
	postDecodeDiagnostics := newTemplateDiagnosticCollector(diagnosticBudget)
	if !normalizeFreeform(&tmpl, postDecodeDiagnostics) || authoring && !promoteLegacyJoins(&tmpl, postDecodeDiagnostics) {
		diagnostics = append(diagnostics, postDecodeDiagnostics.Diagnostics()...)
		return &ParsedTemplate{Template: &tmpl, Diagnostics: diagnostics, SourceHash: hashBytes(data)}, nil
	}
	edges, cardinalityDiagnostics := NormalizeEdgesWithinBudget(&tmpl)
	if !postDecodeDiagnostics.AddAll(cardinalityDiagnostics) {
		diagnostics = append(diagnostics, postDecodeDiagnostics.Diagnostics()...)
		return &ParsedTemplate{Template: &tmpl, Edges: edges, Diagnostics: diagnostics, SourceHash: hashBytes(data)}, nil
	}
	if cardinalityDiagnostics.HasErrors() {
		diagnostics = append(diagnostics, postDecodeDiagnostics.Diagnostics()...)
		return &ParsedTemplate{
			Template: &tmpl, Edges: edges, Diagnostics: diagnostics,
			SourceHash: hashBytes(data),
		}, nil
	}
	validateWithDiagnosticCollector(&tmpl, edges, postDecodeDiagnostics)
	diagnostics = append(diagnostics, postDecodeDiagnostics.Diagnostics()...)
	if postDecodeDiagnostics.Exhausted() {
		return &ParsedTemplate{
			Template: &tmpl, Edges: edges, Diagnostics: diagnostics,
			SourceHash: hashBytes(data),
		}, nil
	}

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
	rawGraphInvalidShape
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
		case rawGraphInvalidShape:
			recordStructuralDiagnostic(invalidGraphShapeDiagnostic(path))
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
		if next.Kind == yaml.ScalarNode {
			target, ok := decodedStructuralScalar(next)
			if !ok {
				recordStructuralIssue(rawGraphInvalidShape, joinPath(nodePath, "next"))
				continue
			}
			if target != "" {
				counts.Edges = saturatingAdd(counts.Edges, 1, MaxNormalizedEdges)
			}
			continue
		}
		if next.Kind != yaml.MappingNode {
			recordStructuralIssue(rawGraphInvalidShape, joinPath(nodePath, "next"))
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
func promoteLegacyJoins(tmpl *Template, diagnostics *templateDiagnosticCollector) bool {
	if tmpl == nil {
		return true
	}
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		node := tmpl.Nodes[nodeID]
		legacy, present := node.Metadata["join"]
		if !present {
			continue
		}
		value, ok := legacy.(string)
		policy := JoinPolicy(value)
		if !ok || policy != JoinAll && policy != JoinAny {
			if !diagnostics.Add(diagError("invalid_legacy_join", "nodes."+nodeID+".metadata.join", "legacy metadata.join must be all or any")) {
				return false
			}
			continue
		}
		if node.Join == "" {
			node.Join = policy
		} else if node.Join != policy {
			if !diagnostics.Add(diagError("join_metadata_conflict", "nodes."+nodeID+".metadata.join",
				fmt.Sprintf("typed join %q disagrees with legacy metadata.join %q", node.Join, policy))) {
				return false
			}
		}
		delete(node.Metadata, "join")
		tmpl.Nodes[nodeID] = node
	}
	return true
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

// freeformMappingKeyIdentity mirrors both stages of scalar-key identity below
// freeform fields: yaml.v3 first resolves keys into a map[any]any, then
// normalizeInterfaceAnyMap stringifies them. Either stage can collide. Keeping
// both identities matters for signed zero: float64(-0) equals float64(0) as a
// Go map key, but stringifies as "-0", which is distinct from integer 0's "0".
type freeformMappingKeyIdentity struct {
	normalized string
	decoded    any
	comparable bool
}

func freeformMappingKeyIdentityOf(node *yaml.Node) freeformMappingKeyIdentity {
	node = resolvedScalarNode(node)
	if node == nil {
		return freeformMappingKeyIdentity{normalized: mappingKeyID(node)}
	}
	var value any
	if err := node.Decode(&value); err != nil {
		return freeformMappingKeyIdentity{normalized: mappingKeyID(node)}
	}
	normalized, ok := value.(string)
	if !ok {
		normalized = fmt.Sprint(value)
	}
	comparable := value == nil
	if valueType := reflect.TypeOf(value); valueType != nil {
		comparable = valueType.Comparable()
	}
	if comparable && value != nil {
		// NaN is comparable but not equal to itself, so Go maps preserve each
		// occurrence as a distinct decoded key. It must not participate in the
		// decoded-key index; normalized string identity still handles any later
		// collision during freeform normalization.
		comparable = value == value
	}
	return freeformMappingKeyIdentity{
		normalized: normalized,
		decoded:    value,
		comparable: comparable,
	}
}

func resolvedScalarNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.ScalarNode {
		return node
	}
	if node.Kind != yaml.AliasNode {
		return nil
	}
	seen := make(map[*yaml.Node]struct{})
	for node.Kind == yaml.AliasNode {
		if _, cycle := seen[node]; cycle {
			return nil
		}
		seen[node] = struct{}{}
		node = node.Alias
		if node == nil {
			return nil
		}
	}
	if node.Kind != yaml.ScalarNode {
		return nil
	}
	return node
}

// decodedStructuralScalar mirrors yaml.v3's Decode-to-string semantics for
// Template.Start and string-keyed graph maps. It follows scalar aliases with
// cycle protection; complex/wrong-kind keys remain the decoder's error.
func decodedStructuralScalar(node *yaml.Node) (string, bool) {
	node = resolvedScalarNode(node)
	if node == nil {
		return "", false
	}
	var value string
	if err := node.Decode(&value); err != nil {
		return "", false
	}
	return value, true
}

type yamlMappingContext uint8

const (
	yamlMappingRoot yamlMappingContext = iota
	yamlMappingParams
	yamlMappingParam
	yamlMappingNodes
	yamlMappingNode
	yamlMappingMetadata
	yamlMappingFreeform
	yamlMappingTyped
)

func yamlMappingChildContext(context yamlMappingContext, key string) yamlMappingContext {
	switch context {
	case yamlMappingRoot:
		switch key {
		case "params":
			return yamlMappingParams
		case "nodes":
			return yamlMappingNodes
		default:
			return yamlMappingTyped
		}
	case yamlMappingParams:
		return yamlMappingParam
	case yamlMappingParam:
		if key == "default" {
			return yamlMappingFreeform
		}
		return yamlMappingTyped
	case yamlMappingNodes:
		return yamlMappingNode
	case yamlMappingNode:
		if key == "metadata" {
			return yamlMappingMetadata
		}
		return yamlMappingTyped
	case yamlMappingMetadata, yamlMappingFreeform:
		return yamlMappingFreeform
	default:
		return yamlMappingTyped
	}
}

func mappingKeyIdentityForContext(node *yaml.Node, context yamlMappingContext) freeformMappingKeyIdentity {
	if context == yamlMappingFreeform {
		return freeformMappingKeyIdentityOf(node)
	}
	return freeformMappingKeyIdentity{normalized: mappingKeyID(node)}
}

type contextualYAMLNode struct {
	node    *yaml.Node
	context yamlMappingContext
}

// pruneDuplicateKeys applies last-wins pruning with immutable copy-on-write
// views. Aliases reused under typed and freeform maps may need different key
// identities, but unchanged subtrees remain shared; only mappings that lose a
// duplicate and the ancestor pointers leading to them are copied.
func pruneDuplicateKeys(root *yaml.Node) *yaml.Node {
	memo := make(map[contextualYAMLNode]*yaml.Node)
	active := make(map[contextualYAMLNode]struct{})
	var walk func(node *yaml.Node, context yamlMappingContext) *yaml.Node
	walk = func(node *yaml.Node, context yamlMappingContext) *yaml.Node {
		if node == nil {
			return nil
		}
		key := contextualYAMLNode{node: node, context: context}
		if result := memo[key]; result != nil {
			return result
		}
		if _, cycle := active[key]; cycle {
			return node
		}
		active[key] = struct{}{}
		defer delete(active, key)

		if node.Kind == yaml.AliasNode {
			target := walk(node.Alias, context)
			if target == node.Alias {
				memo[key] = node
				return node
			}
			out := *node
			out.Alias = target
			memo[key] = &out
			return &out
		}
		if node.Kind == yaml.MappingNode {
			// YAML convention is last-wins for duplicate keys; keep each key at
			// its final occurrence so the decoded template (and its semantic
			// hash) match standard YAML semantics.
			lastNormalized := map[string]int{}
			lastDecoded := map[any]int{}
			for i := 0; i < len(node.Content); i += 2 {
				identity := mappingKeyIdentityForContext(node.Content[i], context)
				lastNormalized[identity.normalized] = i
				if identity.comparable {
					lastDecoded[identity.decoded] = i
				}
			}
			var pruned []*yaml.Node
			for i := 0; i < len(node.Content); i += 2 {
				identity := mappingKeyIdentityForContext(node.Content[i], context)
				duplicate := lastNormalized[identity.normalized] != i
				if identity.comparable && lastDecoded[identity.decoded] != i {
					duplicate = true
				}
				if duplicate {
					if pruned == nil {
						pruned = append(make([]*yaml.Node, 0, len(node.Content)), node.Content[:i]...)
					}
					continue
				}
				value := walk(node.Content[i+1], yamlMappingChildContext(context, identity.normalized))
				if value != node.Content[i+1] && pruned == nil {
					pruned = append(make([]*yaml.Node, 0, len(node.Content)), node.Content[:i]...)
				}
				if pruned != nil {
					pruned = append(pruned, node.Content[i], value)
				}
			}
			if pruned == nil {
				memo[key] = node
				return node
			}
			out := *node
			out.Content = pruned
			memo[key] = &out
			return &out
		}
		if node.Kind == yaml.DocumentNode || node.Kind == yaml.SequenceNode {
			var content []*yaml.Node
			for i, child := range node.Content {
				result := walk(child, context)
				if result != child && content == nil {
					content = append([]*yaml.Node(nil), node.Content...)
				}
				if content != nil {
					content[i] = result
				}
			}
			if content != nil {
				out := *node
				out.Content = content
				memo[key] = &out
				return &out
			}
		}
		memo[key] = node
		return node
	}
	return walk(root, yamlMappingRoot)
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
	var root yaml.Node
	if err := root.Encode(&clone); err != nil {
		return nil, fmt.Errorf("canonicalize process template YAML: %w", err)
	}
	if err := restoreCanonicalYAMLStrings(&root, reflect.ValueOf(&clone)); err != nil {
		return nil, fmt.Errorf("canonicalize process template YAML: %w", err)
	}
	quoteAmbiguousCanonicalYAMLKeys(&root)
	data, err := yaml.Marshal(&root)
	if err != nil {
		return nil, fmt.Errorf("canonicalize process template YAML: %w", err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	return data, nil
}

var nextReflectType = reflect.TypeFor[Next]()

// restoreCanonicalYAMLStrings repairs a yaml.v3 node tree against the modeled
// Go value. yaml.Node.Encode internally emits and reparses YAML, which can
// normalize line endings or lose a leading newline in literal-block strings.
// Restoring the exact value and quoting any scalar changed by that internal
// round trip keeps CanonicalYAML lossless.
func restoreCanonicalYAMLStrings(node *yaml.Node, value reflect.Value) error {
	for value.IsValid() && (value.Kind() == reflect.Pointer || value.Kind() == reflect.Interface) {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}
	if node == nil || !value.IsValid() {
		return nil
	}
	if value.Type() == nextReflectType && node.Kind == yaml.ScalarNode {
		target := value.MapIndex(reflect.ValueOf(DefaultOutcome))
		if target.IsValid() {
			return restoreCanonicalYAMLStrings(node, target)
		}
		return nil
	}
	switch value.Kind() {
	case reflect.String:
		if node.Kind != yaml.ScalarNode {
			return nil
		}
		if !utf8.ValidString(value.String()) {
			// Node.Encode already represented this modeled string safely as a
			// binary scalar. Replacing it with raw invalid UTF-8 tagged !!str
			// would make the final yaml.Marshal fail.
			return nil
		}
		modeled := value.String()
		changedByEncode := node.Value != modeled
		node.Value = modeled
		node.Tag = "!!str"
		if changedByEncode || strings.HasPrefix(node.Value, "\n") {
			node.Style = yaml.DoubleQuotedStyle
		}
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return nil
		}
		valueType := value.Type()
		for i := 0; i < value.NumField(); i++ {
			fieldType := valueType.Field(i)
			if !fieldType.IsExported() {
				continue
			}
			name := strings.Split(fieldType.Tag.Get("yaml"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = strings.ToLower(fieldType.Name)
			}
			if child := yamlMappingValueNode(node, name); child != nil {
				if err := restoreCanonicalYAMLStrings(child, value.Field(i)); err != nil {
					return err
				}
			}
		}
	case reflect.Map:
		if node.Kind != yaml.MappingNode || value.Type().Key().Kind() != reflect.String {
			return nil
		}
		keys, err := canonicalYAMLStringMapKeys(value)
		if err != nil {
			return err
		}
		for nodeIndex := 0; nodeIndex+1 < len(node.Content); nodeIndex += 2 {
			keyNode := node.Content[nodeIndex]
			key, ok := keys[canonicalYAMLScalarFingerprintOf(keyNode)]
			if !ok {
				return fmt.Errorf("cannot associate encoded map key tag=%q value=%q", keyNode.Tag, keyNode.Value)
			}
			mapValue := value.MapIndex(key)
			if !mapValue.IsValid() {
				return fmt.Errorf("cannot find modeled map key %q", key.String())
			}
			if err := restoreCanonicalYAMLStrings(keyNode, key); err != nil {
				return err
			}
			if err := restoreCanonicalYAMLStrings(node.Content[nodeIndex+1], mapValue); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if node.Kind != yaml.SequenceNode {
			return nil
		}
		for i := 0; i < value.Len() && i < len(node.Content); i++ {
			if err := restoreCanonicalYAMLStrings(node.Content[i], value.Index(i)); err != nil {
				return err
			}
		}
	}
	return nil
}

type canonicalYAMLScalarFingerprint struct {
	tag   string
	value string
	style yaml.Style
}

func canonicalYAMLScalarFingerprintOf(node *yaml.Node) canonicalYAMLScalarFingerprint {
	return canonicalYAMLScalarFingerprint{tag: node.Tag, value: node.Value, style: node.Style}
}

// canonicalYAMLStringMapKeys matches each modeled key by the scalar yaml.v3
// itself emits for that string. Unlike comparing a second map's iteration
// order, this remains correct when invalid UTF-8 keys compare equal as runes.
func canonicalYAMLStringMapKeys(value reflect.Value) (map[canonicalYAMLScalarFingerprint]reflect.Value, error) {
	keys := make(map[canonicalYAMLScalarFingerprint]reflect.Value, value.Len())
	for _, key := range value.MapKeys() {
		var encoded yaml.Node
		if err := encoded.Encode(key.Interface()); err != nil {
			return nil, fmt.Errorf("encode modeled map key %q: %w", key.String(), err)
		}
		fingerprint := canonicalYAMLScalarFingerprintOf(&encoded)
		if previous, duplicate := keys[fingerprint]; duplicate {
			return nil, fmt.Errorf("modeled map keys %q and %q have the same YAML scalar representation", previous.String(), key.String())
		}
		keys[fingerprint] = key
	}
	return keys, nil
}

func yamlMappingValueNode(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// quoteAmbiguousCanonicalYAMLKeys makes the string identity of canonical map
// keys explicit whenever their plain spelling would resolve to a non-string
// YAML tag. This is especially important after freeform-key normalization:
// reparsing must not turn the modeled string key back into an int, bool, null,
// float, or timestamp before duplicate pruning and decoding run again.
func quoteAmbiguousCanonicalYAMLKeys(node *yaml.Node) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			quoteAmbiguousCanonicalYAMLKeys(child)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind == yaml.ScalarNode && key.Tag == "!!str" {
				probe := yaml.Node{Kind: yaml.ScalarNode, Value: key.Value}
				if probe.ShortTag() != "!!str" {
					key.Style = yaml.DoubleQuotedStyle
				}
			}
			quoteAmbiguousCanonicalYAMLKeys(key)
			quoteAmbiguousCanonicalYAMLKeys(node.Content[i+1])
		}
	}
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
		if tmpl.Layout.Edges != nil {
			// Both levels are copied: sharing the inner maps would let a mutation
			// through the clone reach the original's pin state.
			layout.Edges = make(map[string]map[string]LayoutEdge, len(tmpl.Layout.Edges))
			for from, outcomes := range tmpl.Layout.Edges {
				copied := make(map[string]LayoutEdge, len(outcomes))
				for outcome, value := range outcomes {
					copied[outcome] = value
				}
				layout.Edges[from] = copied
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
	visited := make(map[contextualYAMLNode]struct{})
	var walk func(node *yaml.Node, path string, context yamlMappingContext)
	walk = func(node *yaml.Node, path string, context yamlMappingContext) {
		if node == nil || budget.exhausted {
			return
		}
		if node.Kind == yaml.AliasNode {
			walk(node.Alias, path, context)
			return
		}
		key := contextualYAMLNode{node: node, context: context}
		if _, ok := visited[key]; ok {
			return
		}
		visited[key] = struct{}{}
		if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
			walk(node.Content[0], path, context)
			return
		}
		if node.Kind == yaml.MappingNode {
			seenNormalized := map[string]struct{}{}
			seenDecoded := map[any]struct{}{}
			for i := 0; i < len(node.Content); i += 2 {
				if budget.exhausted {
					break
				}
				key := node.Content[i]
				value := node.Content[i+1]
				identity := mappingKeyIdentityForContext(key, context)
				keyPath := joinPath(path, identity.normalized)
				_, duplicate := seenNormalized[identity.normalized]
				if identity.comparable {
					if _, ok := seenDecoded[identity.decoded]; ok {
						duplicate = true
					}
				}
				if duplicate {
					budget.append(&diagnostics, diagErrorAt("duplicate_key", keyPath, "duplicate YAML mapping key", key))
				}
				seenNormalized[identity.normalized] = struct{}{}
				if identity.comparable {
					seenDecoded[identity.decoded] = struct{}{}
				}
				walk(value, keyPath, yamlMappingChildContext(context, identity.normalized))
			}
			return
		}
		if node.Kind == yaml.SequenceNode {
			for i, child := range node.Content {
				walk(child, fmt.Sprintf("%s[%d]", path, i), context)
			}
		}
	}
	walk(root, "", yamlMappingRoot)
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
