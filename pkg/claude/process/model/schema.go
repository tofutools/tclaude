package model

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

var (
	templateFields  = stringSet("apiVersion", "kind", "id", "name", "description", "doc", "params", "start", "nodes", "layout")
	paramFields     = stringSet("type", "name", "description", "doc", "required", "default")
	nodeFields      = stringSet("type", "name", "description", "doc", "performer", "plan", "checks", "review", "retry", "wait", "next", "result", "metadata")
	stepFields      = stringSet("id", "name", "description", "doc", "performer", "approval", "approvalRetry", "retry")
	performerFields = stringSet(
		"kind",
		"profile",
		"prompt",
		"ask",
		"run",
		"args",
		"timeout",
	)
	retryFields      = stringSet("maxAttempts", "backoff", "onFail")
	waitFields       = stringSet("duration", "until", "signal")
	layoutFields     = stringSet("nodes")
	layoutNodeFields = stringSet("x", "y")
)

func unknownFieldDiagnostics(root *yaml.Node) Diagnostics {
	if root == nil {
		return nil
	}
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return nil
		}
		root = root.Content[0]
	}
	root = resolveAlias(root)
	if root == nil || root.Kind != yaml.MappingNode {
		return nil
	}
	return checkKnownFields(root, "", templateFields, templateChildSchema)
}

// resolveAlias follows YAML alias nodes to the anchored node they reference.
// The raw-node walk runs before Decode, where aliases are still unresolved, so
// alias-defined mappings/sequences would otherwise slip past the schema check.
func resolveAlias(node *yaml.Node) *yaml.Node {
	for node != nil && node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	return node
}

func templateChildSchema(key string) schemaFunc {
	switch key {
	case "params":
		return mapValuesSchema(paramFields, paramChildSchema)
	case "nodes":
		return mapValuesSchema(nodeFields, nodeChildSchema)
	case "layout":
		return namedSchema(layoutFields, layoutChildSchema)
	default:
		return nil
	}
}

func paramChildSchema(key string) schemaFunc {
	if key == "default" {
		return freeformSchema
	}
	return nil
}

func nodeChildSchema(key string) schemaFunc {
	switch key {
	case "performer":
		return namedSchema(performerFields, nil)
	case "plan", "review":
		return namedSchema(stepFields, stepChildSchema)
	case "checks":
		return sequenceValuesSchema(stepFields, stepChildSchema)
	case "retry":
		return namedSchema(retryFields, nil)
	case "wait":
		return namedSchema(waitFields, nil)
	case "next":
		return nextSchema
	case "metadata":
		return freeformSchema
	default:
		return nil
	}
}

// nextSchema flags YAML constructs the next-edge decoder cannot represent. Merge
// keys (`<<`) are the notable case: Next.UnmarshalYAML skips them, so without a
// diagnostic they would silently drop outcome edges.
func nextSchema(node *yaml.Node, path string) Diagnostics {
	node = resolveAlias(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	var diagnostics Diagnostics
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.ShortTag() == mergeTag {
			diagnostics = append(diagnostics, mergeKeyDiag(key, path))
		}
	}
	return diagnostics
}

// mergeKeyDiag reports a `<<` merge key. The model supports merge keys in no
// mapping: Decode would silently apply the merge (or drop it, for next), so the
// walk rejects it explicitly rather than mislabeling it an unknown field.
func mergeKeyDiag(key *yaml.Node, path string) Diagnostic {
	return diagErrorAt("merge_key_unsupported", joinPath(path, "<<"),
		"merge keys (<<) are not supported; declare fields explicitly", key)
}

func stepChildSchema(key string) schemaFunc {
	switch key {
	case "performer":
		return namedSchema(performerFields, nil)
	case "approvalRetry", "retry":
		return namedSchema(retryFields, nil)
	default:
		return nil
	}
}

func layoutChildSchema(key string) schemaFunc {
	if key == "nodes" {
		return mapValuesSchema(layoutNodeFields, nil)
	}
	return nil
}

type schemaFunc func(node *yaml.Node, path string) Diagnostics

func namedSchema(allowed map[string]struct{}, child func(string) schemaFunc) schemaFunc {
	return func(node *yaml.Node, path string) Diagnostics {
		return checkKnownFields(node, path, allowed, child)
	}
}

func mapValuesSchema(allowed map[string]struct{}, child func(string) schemaFunc) schemaFunc {
	return func(node *yaml.Node, path string) Diagnostics {
		node = resolveAlias(node)
		if node == nil || node.Kind != yaml.MappingNode {
			return nil
		}
		var diagnostics Diagnostics
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			value := node.Content[i+1]
			if key.ShortTag() == mergeTag {
				diagnostics = append(diagnostics, mergeKeyDiag(key, path))
				continue
			}
			diagnostics = append(diagnostics, checkKnownFields(value, joinPath(path, key.Value), allowed, child)...)
		}
		return diagnostics
	}
}

func sequenceValuesSchema(allowed map[string]struct{}, child func(string) schemaFunc) schemaFunc {
	return func(node *yaml.Node, path string) Diagnostics {
		node = resolveAlias(node)
		if node == nil || node.Kind != yaml.SequenceNode {
			return nil
		}
		var diagnostics Diagnostics
		for i, childNode := range node.Content {
			diagnostics = append(diagnostics, checkKnownFields(childNode, fmt.Sprintf("%s[%d]", path, i), allowed, child)...)
		}
		return diagnostics
	}
}

func freeformSchema(_ *yaml.Node, _ string) Diagnostics {
	return nil
}

func checkKnownFields(node *yaml.Node, path string, allowed map[string]struct{}, child func(string) schemaFunc) Diagnostics {
	node = resolveAlias(node)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	var diagnostics Diagnostics
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		value := node.Content[i+1]
		if key.ShortTag() == mergeTag {
			diagnostics = append(diagnostics, mergeKeyDiag(key, path))
			continue
		}
		keyPath := joinPath(path, key.Value)
		if _, ok := allowed[key.Value]; !ok {
			diagnostics = append(diagnostics, diagErrorAt("unknown_field", keyPath, fmt.Sprintf("unknown field %q", key.Value), key))
			continue
		}
		if child != nil {
			if schema := child(key.Value); schema != nil {
				diagnostics = append(diagnostics, schema(value, keyPath)...)
			}
		}
	}
	return diagnostics
}

func stringSet(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}
