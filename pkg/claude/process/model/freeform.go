package model

import "fmt"

func normalizeFreeform(tmpl *Template) Diagnostics {
	if tmpl == nil {
		return nil
	}
	var diagnostics Diagnostics
	for paramID, param := range tmpl.Params {
		value, diags := normalizeAny(param.Default, "params."+paramID+".default")
		diagnostics = append(diagnostics, diags...)
		param.Default = value
		tmpl.Params[paramID] = param
	}
	for nodeID, node := range tmpl.Nodes {
		value, diags := normalizeAny(map[string]any(node.Metadata), "nodes."+nodeID+".metadata")
		diagnostics = append(diagnostics, diags...)
		if value == nil {
			node.Metadata = nil
		} else {
			if metadata, ok := value.(map[string]any); ok {
				node.Metadata = Metadata(metadata)
			}
		}
		tmpl.Nodes[nodeID] = node
	}
	return diagnostics
}

func normalizeAny(value any, path string) (any, Diagnostics) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return normalizeStringAnyMap(typed, path)
	case map[any]any:
		return normalizeInterfaceAnyMap(typed, path)
	case []any:
		out := make([]any, len(typed))
		var diagnostics Diagnostics
		for i, item := range typed {
			normalized, diags := normalizeAny(item, fmt.Sprintf("%s[%d]", path, i))
			diagnostics = append(diagnostics, diags...)
			out[i] = normalized
		}
		return out, diagnostics
	default:
		return value, nil
	}
}

func normalizeStringAnyMap(values map[string]any, path string) (any, Diagnostics) {
	out := make(map[string]any, len(values))
	var diagnostics Diagnostics
	for _, key := range sortedKeys(values) {
		value, diags := normalizeAny(values[key], joinPath(path, key))
		diagnostics = append(diagnostics, diags...)
		out[key] = value
	}
	return out, diagnostics
}

func normalizeInterfaceAnyMap(values map[any]any, path string) (any, Diagnostics) {
	out := make(map[string]any, len(values))
	var diagnostics Diagnostics
	keys := sortedAnyKeys(values)
	for _, key := range keys {
		stringKey, ok := key.(string)
		if !ok {
			stringKey = fmt.Sprint(key)
			diagnostics = append(diagnostics, diagError("non_string_freeform_key", joinPath(path, stringKey), "freeform map keys must be strings"))
		}
		value, diags := normalizeAny(values[key], joinPath(path, stringKey))
		diagnostics = append(diagnostics, diags...)
		out[stringKey] = value
	}
	return out, diagnostics
}

func sortedAnyKeys(values map[any]any) []any {
	keys := make([]any, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sortAnyValues(keys)
	return keys
}
