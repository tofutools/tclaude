package model

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	templateFields   = stringSet("apiVersion", "kind", "id", "name", "description", "doc", "params", "start", "nodes", "layout")
	paramFields      = stringSet("type", "name", "description", "doc", "required", "default")
	nodeFields       = stringSet("type", "join", "name", "description", "doc", "performer", "plan", "checks", "review", "retry", "wait", "next", "result", "captures", "metadata")
	stepFields       = stringSet("id", "name", "description", "doc", "performer", "approval", "approvalRetry", "retry")
	performerFields  = stringSet("kind", "profile", "prompt", "ask", "choices", "choiceOutcomes", "assignee", "model", "effort", "run", "args", "timeout", "contact")
	contactFields    = stringSet("cadence", "budget", "escalationTarget")
	retryFields      = stringSet("maxAttempts", "backoff", "onFail")
	waitFields       = stringSet("duration", "until", "signal")
	layoutFields     = stringSet("nodes")
	layoutNodeFields = stringSet("x", "y")
)

type schemaID uint8

const (
	schemaTemplate schemaID = iota
	schemaParamMap
	schemaParam
	schemaNodeMap
	schemaNode
	schemaLayout
	schemaLayoutNodeMap
	schemaLayoutNode
	schemaStep
	schemaChecks
	schemaPerformer
	schemaContact
	schemaRetry
	schemaWait
	schemaNext
	schemaCount
)

type schemaMemoKey struct {
	node   *yaml.Node
	schema schemaID
}

type schemaWalk struct {
	maximumAliasSteps int
	memo              map[schemaMemoKey]Diagnostics
	active            map[schemaMemoKey]struct{}
	definitionBudget  templateDiagnosticBudget
	compositionCount  int
	compositionWire   int
	outputBudget      *templateDiagnosticBudget
	exhausted         bool
}

func unknownFieldDiagnostics(root *yaml.Node, outputBudget *templateDiagnosticBudget) Diagnostics {
	if root == nil {
		return nil
	}
	walk := &schemaWalk{
		maximumAliasSteps: yamlTreeNodeCount(root),
		memo:              make(map[schemaMemoKey]Diagnostics),
		active:            make(map[schemaMemoKey]struct{}),
		outputBudget:      outputBudget,
	}
	relative := walk.inspect(root, schemaTemplate)
	diagnostics := walk.instantiate("", relative)
	if walk.exhausted || outputBudget.exhausted {
		diagnostics = append(diagnostics, templateDiagnosticBudgetDiagnostic())
	}
	return diagnostics
}

func mergeKeyDiag(key *yaml.Node, path string) Diagnostic {
	return diagErrorAt("merge_key_unsupported", joinPath(path, "<<"),
		"merge keys (<<) are not supported; declare fields explicitly", key)
}

// inspect memoizes relative findings by source-node identity and schema
// context. Repeated aliases retain distinct occurrence paths when instantiated,
// but a large valid shared subtree is traversed only once.
func (w *schemaWalk) inspect(node *yaml.Node, schema schemaID) Diagnostics {
	if w.exhausted || node == nil {
		return nil
	}
	node, status := structuralNode(node, w.maximumAliasSteps)
	if status == rawGraphAliasUnsafe {
		w.exhausted = true
		return nil
	}
	if status != rawGraphCounted {
		return nil
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil
		}
		return w.inspect(node.Content[0], schema)
	}
	key := schemaMemoKey{node: node, schema: schema}
	if diagnostics, ok := w.memo[key]; ok {
		return diagnostics
	}
	if _, cycle := w.active[key]; cycle {
		w.exhausted = true
		return nil
	}
	w.active[key] = struct{}{}
	var diagnostics Diagnostics
	switch schema {
	case schemaParamMap:
		diagnostics = w.inspectMapValues(node, schemaParam)
	case schemaNodeMap:
		diagnostics = w.inspectMapValues(node, schemaNode)
	case schemaLayoutNodeMap:
		diagnostics = w.inspectMapValues(node, schemaLayoutNode)
	case schemaChecks:
		diagnostics = w.inspectSequence(node, schemaStep)
	case schemaNext:
		diagnostics = w.inspectNext(node)
	default:
		diagnostics = w.inspectKnown(node, schema)
	}
	delete(w.active, key)
	w.memo[key] = diagnostics
	return diagnostics
}

func (w *schemaWalk) inspectKnown(node *yaml.Node, schema schemaID) Diagnostics {
	allowed := schemaAllowedFields(schema)
	if allowed == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	var diagnostics Diagnostics
	for i := 0; i+1 < len(node.Content) && !w.exhausted; i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.ShortTag() == mergeTag {
			diagnostics = append(diagnostics, w.makeDiagnostic("merge_key_unsupported", "<<",
				"merge keys (<<) are not supported; declare fields explicitly", key)...)
			continue
		}
		keyValue := mappingKeyID(key)
		if _, ok := allowed[keyValue]; !ok {
			messageBytes := len(`unknown field ""`) + len(keyValue)
			if !w.definitionBudget.fits(len("unknown_field"), len(keyValue), messageBytes) {
				w.exhausted = true
				break
			}
			diagnostic := diagErrorAt("unknown_field", keyValue, fmt.Sprintf("unknown field %q", keyValue), key)
			if !w.definitionBudget.append(&diagnostics, diagnostic) {
				w.exhausted = true
				break
			}
			continue
		}
		if child, ok := schemaChild(schema, keyValue); ok {
			diagnostics = append(diagnostics, w.prefixRelative(keyValue, w.inspect(value, child))...)
		}
	}
	return diagnostics
}

func (w *schemaWalk) inspectMapValues(node *yaml.Node, child schemaID) Diagnostics {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	var diagnostics Diagnostics
	for i := 0; i+1 < len(node.Content) && !w.exhausted; i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.ShortTag() == mergeTag {
			diagnostics = append(diagnostics, w.makeDiagnostic("merge_key_unsupported", "<<",
				"merge keys (<<) are not supported; declare fields explicitly", key)...)
			continue
		}
		diagnostics = append(diagnostics, w.prefixRelative(mappingKeyID(key), w.inspect(value, child))...)
	}
	return diagnostics
}

func (w *schemaWalk) inspectSequence(node *yaml.Node, child schemaID) Diagnostics {
	if node.Kind != yaml.SequenceNode {
		return nil
	}
	var diagnostics Diagnostics
	for i, childNode := range node.Content {
		if w.exhausted {
			break
		}
		diagnostics = append(diagnostics, w.prefixRelative(fmt.Sprintf("[%d]", i), w.inspect(childNode, child))...)
	}
	return diagnostics
}

func (w *schemaWalk) inspectNext(node *yaml.Node) Diagnostics {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	var diagnostics Diagnostics
	for i := 0; i+1 < len(node.Content) && !w.exhausted; i += 2 {
		key := node.Content[i]
		if key.ShortTag() == mergeTag {
			diagnostics = append(diagnostics, w.makeDiagnostic("merge_key_unsupported", "<<",
				"merge keys (<<) are not supported; declare fields explicitly", key)...)
		}
	}
	return diagnostics
}

func (w *schemaWalk) makeDiagnostic(code, path, message string, node *yaml.Node) Diagnostics {
	var diagnostics Diagnostics
	if !w.definitionBudget.append(&diagnostics, diagErrorAt(code, path, message, node)) {
		w.exhausted = true
		return nil
	}
	return diagnostics
}

func (w *schemaWalk) instantiate(prefix string, relative Diagnostics) Diagnostics {
	var diagnostics Diagnostics
	for _, diagnostic := range relative {
		path := prefixSchemaPath(prefix, diagnostic.Path)
		diagnostic.Path = path
		if !w.outputBudget.append(&diagnostics, diagnostic) {
			w.exhausted = true
			break
		}
	}
	return diagnostics
}

func (w *schemaWalk) prefixRelative(prefix string, diagnostics Diagnostics) Diagnostics {
	if prefix == "" || len(diagnostics) == 0 {
		return diagnostics
	}
	maximumCompositionSteps := MaxTemplateAuthoringDiagnostics * int(schemaCount)
	maximumCompositionWire := MaxTemplateDiagnosticWireBytes * int(schemaCount)
	out := make(Diagnostics, 0, min(len(diagnostics), maximumCompositionSteps-w.compositionCount))
	for _, diagnostic := range diagnostics {
		separatorBytes := 1
		if diagnostic.Path == "" || strings.HasPrefix(diagnostic.Path, "[") {
			separatorBytes = 0
		}
		pathBytes := len(prefix) + separatorBytes + len(diagnostic.Path)
		wireBytes := templateDiagnosticWireCost(len(diagnostic.Code), pathBytes, len(diagnostic.Message))
		if wireBytes > MaxTemplateDiagnosticWireBytes ||
			w.compositionCount >= maximumCompositionSteps ||
			wireBytes > maximumCompositionWire-w.compositionWire {
			w.exhausted = true
			break
		}
		diagnostic.Path = prefixSchemaPath(prefix, diagnostic.Path)
		out = append(out, diagnostic)
		w.compositionCount++
		w.compositionWire += wireBytes
	}
	return out
}

func prefixSchemaPath(prefix, path string) string {
	if prefix == "" {
		return path
	}
	if path == "" {
		return prefix
	}
	if strings.HasPrefix(path, "[") {
		return prefix + path
	}
	return prefix + "." + path
}

func schemaAllowedFields(schema schemaID) map[string]struct{} {
	switch schema {
	case schemaTemplate:
		return templateFields
	case schemaParam:
		return paramFields
	case schemaNode:
		return nodeFields
	case schemaLayout:
		return layoutFields
	case schemaLayoutNode:
		return layoutNodeFields
	case schemaStep:
		return stepFields
	case schemaPerformer:
		return performerFields
	case schemaContact:
		return contactFields
	case schemaRetry:
		return retryFields
	case schemaWait:
		return waitFields
	default:
		return nil
	}
}

func schemaChild(schema schemaID, key string) (schemaID, bool) {
	switch schema {
	case schemaTemplate:
		switch key {
		case "params":
			return schemaParamMap, true
		case "nodes":
			return schemaNodeMap, true
		case "layout":
			return schemaLayout, true
		}
	case schemaNode:
		switch key {
		case "performer":
			return schemaPerformer, true
		case "plan", "review":
			return schemaStep, true
		case "checks":
			return schemaChecks, true
		case "retry":
			return schemaRetry, true
		case "wait":
			return schemaWait, true
		case "next":
			return schemaNext, true
		}
	case schemaLayout:
		if key == "nodes" {
			return schemaLayoutNodeMap, true
		}
	case schemaStep:
		switch key {
		case "performer":
			return schemaPerformer, true
		case "approvalRetry", "retry":
			return schemaRetry, true
		}
	case schemaPerformer:
		if key == "contact" {
			return schemaContact, true
		}
	}
	return 0, false
}

func stringSet(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}
