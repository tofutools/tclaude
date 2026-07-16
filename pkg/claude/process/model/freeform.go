package model

import "fmt"

func normalizeFreeform(tmpl *Template, diagnostics *templateDiagnosticCollector) bool {
	if tmpl == nil {
		return true
	}
	for _, paramID := range sortedKeys(tmpl.Params) {
		param := tmpl.Params[paramID]
		value, ok := normalizeAny(param.Default, "params."+paramID+".default", diagnostics)
		if !ok {
			return false
		}
		param.Default = value
		tmpl.Params[paramID] = param
	}
	for _, nodeID := range sortedKeys(tmpl.Nodes) {
		node := tmpl.Nodes[nodeID]
		value, ok := normalizeAny(map[string]any(node.Metadata), "nodes."+nodeID+".metadata", diagnostics)
		if !ok {
			return false
		}
		if value == nil {
			node.Metadata = nil
		} else {
			if metadata, ok := value.(map[string]any); ok {
				node.Metadata = Metadata(metadata)
			}
		}
		tmpl.Nodes[nodeID] = node
	}
	return true
}

func normalizeAny(value any, path string, diagnostics *templateDiagnosticCollector) (any, bool) {
	switch typed := value.(type) {
	case nil:
		return nil, true
	case map[string]any:
		return normalizeStringAnyMap(typed, path, diagnostics)
	case map[any]any:
		return normalizeInterfaceAnyMap(typed, path, diagnostics)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			normalized, ok := normalizeAny(item, fmt.Sprintf("%s[%d]", path, i), diagnostics)
			if !ok {
				return nil, false
			}
			out[i] = normalized
		}
		return out, true
	default:
		return value, true
	}
}

func normalizeStringAnyMap(values map[string]any, path string, diagnostics *templateDiagnosticCollector) (any, bool) {
	out := make(map[string]any, len(values))
	for _, key := range sortedKeys(values) {
		value, ok := normalizeAny(values[key], joinPath(path, key), diagnostics)
		if !ok {
			return nil, false
		}
		out[key] = value
	}
	return out, true
}

func normalizeInterfaceAnyMap(values map[any]any, path string, diagnostics *templateDiagnosticCollector) (any, bool) {
	out := make(map[string]any, len(values))
	keys := sortedAnyKeys(values)
	for _, key := range keys {
		stringKey, ok := key.(string)
		if !ok {
			stringKey = fmt.Sprint(key)
			if !diagnostics.Add(diagError("non_string_freeform_key", joinPath(path, stringKey), "freeform map keys must be strings")) {
				return nil, false
			}
		}
		value, ok := normalizeAny(values[key], joinPath(path, stringKey), diagnostics)
		if !ok {
			return nil, false
		}
		out[stringKey] = value
	}
	return out, true
}

func sortedAnyKeys(values map[any]any) []any {
	keys := make([]any, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sortAnyValues(keys)
	return keys
}
