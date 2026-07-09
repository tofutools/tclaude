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

func Parse(data []byte) (*ParsedTemplate, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse process template YAML: %w", err)
	}

	diagnostics := duplicateKeyDiagnostics(&root)
	pruneDuplicateKeys(&root)

	var tmpl Template
	if err := root.Decode(&tmpl); err != nil {
		return nil, fmt.Errorf("decode process template: %w", err)
	}

	normalizeTemplate(&tmpl)
	edges := NormalizeEdges(&tmpl)
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
			seen := map[string]struct{}{}
			pruned := make([]*yaml.Node, 0, len(node.Content))
			for i := 0; i < len(node.Content); i += 2 {
				key := node.Content[i]
				value := node.Content[i+1]
				if _, ok := seen[key.Value]; ok {
					continue
				}
				seen[key.Value] = struct{}{}
				pruned = append(pruned, key, value)
				walk(value)
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

func duplicateKeyDiagnostics(root *yaml.Node) Diagnostics {
	var diagnostics Diagnostics
	var walk func(node *yaml.Node, path string)
	walk = func(node *yaml.Node, path string) {
		if node == nil {
			return
		}
		if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
			walk(node.Content[0], path)
			return
		}
		if node.Kind == yaml.MappingNode {
			seen := map[string]struct{}{}
			for i := 0; i < len(node.Content); i += 2 {
				key := node.Content[i]
				value := node.Content[i+1]
				keyPath := joinPath(path, key.Value)
				if _, ok := seen[key.Value]; ok {
					diagnostics = append(diagnostics, diagError("duplicate_key", keyPath, "duplicate YAML mapping key"))
				}
				seen[key.Value] = struct{}{}
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
