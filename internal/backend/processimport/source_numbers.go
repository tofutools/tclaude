package processimport

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/processimport/legacy"
	"gopkg.in/yaml.v3"
)

// Preserve parameter JSON directly from the bounded source tree. The retained
// legacy decoder is still authoritative for authoring validation; interface{}
// floating-point values are not used as a serialization intermediate.
func exactSourceDefaults(source string, template *legacy.Template) (map[string]json.RawMessage, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(source), &root); err != nil {
		return nil, err
	}
	if len(root.Content) != 1 {
		return nil, fmt.Errorf("one source document required")
	}
	params, err := sourceMapValue(root.Content[0], "params")
	if err != nil {
		return nil, err
	}
	visited := 0
	var encode func(*yaml.Node, int) (json.RawMessage, error)
	encode = func(node *yaml.Node, depth int) (json.RawMessage, error) {
		if node == nil {
			return nil, nil
		}
		visited++
		if visited > 100000 || depth > 128 {
			return nil, fmt.Errorf("parameter source expansion exceeds conversion bounds")
		}
		switch node.Kind {
		case yaml.AliasNode:
			return encode(node.Alias, depth+1)
		case yaml.ScalarNode:
			if node.ShortTag() == "!!int" || node.ShortTag() == "!!float" {
				return sourceJSONNumber(node)
			}
			var value any
			if err := node.Decode(&value); err != nil {
				return nil, err
			}
			return json.Marshal(value)
		case yaml.SequenceNode:
			values := make([]json.RawMessage, 0, len(node.Content))
			for _, child := range node.Content {
				value, err := encode(child, depth+1)
				if err != nil {
					return nil, err
				}
				values = append(values, value)
			}
			return json.Marshal(values)
		case yaml.MappingNode:
			values := map[string]json.RawMessage{}
			// YAML merge sequences give earlier maps precedence; explicit keys win
			// over every merge regardless of their source position.
			for i := 0; i+1 < len(node.Content); i += 2 {
				if node.Content[i].ShortTag() != "!!merge" {
					continue
				}
				raw, err := encode(node.Content[i+1], depth+1)
				if err != nil {
					return nil, err
				}
				var maps []map[string]json.RawMessage
				if len(raw) > 0 && raw[0] == '[' {
					if err := json.Unmarshal(raw, &maps); err != nil {
						return nil, err
					}
				} else {
					var m map[string]json.RawMessage
					if err := json.Unmarshal(raw, &m); err != nil {
						return nil, err
					}
					maps = append(maps, m)
				}
				for _, m := range maps {
					for key, value := range m {
						if _, exists := values[key]; !exists {
							values[key] = value
						}
					}
				}
			}
			for i := 0; i+1 < len(node.Content); i += 2 {
				if node.Content[i].ShortTag() == "!!merge" {
					continue
				}
				var key string
				if err := node.Content[i].Decode(&key); err != nil {
					return nil, err
				}
				value, err := encode(node.Content[i+1], depth+1)
				if err != nil {
					return nil, err
				}
				values[key] = value
			}
			return json.Marshal(values)
		default:
			return nil, fmt.Errorf("unsupported parameter source node")
		}
	}
	defaults := map[string]json.RawMessage{}
	for key, parameter := range template.Params {
		row, err := sourceMapValue(params, key)
		if err != nil {
			return nil, err
		}
		value, err := sourceMapValue(row, "default")
		if err != nil {
			return nil, err
		}
		if value == nil && parameter.Default != nil {
			return nil, fmt.Errorf("parameter %s/default: cannot resolve retained source default", key)
		}
		raw, err := encode(value, 0)
		if err != nil {
			return nil, fmt.Errorf("parameter %s/default: %w", key, err)
		}
		if value != nil {
			defaults[key] = raw
		}
	}
	return defaults, nil
}

func sourceMapValue(node *yaml.Node, key string) (*yaml.Node, error) {
	for depth := 0; node != nil && node.Kind == yaml.AliasNode; depth++ {
		if depth >= 128 {
			return nil, fmt.Errorf("parameter source alias expansion exceeds conversion bounds")
		}
		node = node.Alias
	}
	if node == nil || node.Kind != yaml.MappingNode {
		return nil, nil
	}
	var result *yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		var decoded string
		if node.Content[i].Decode(&decoded) == nil && decoded == key {
			result = node.Content[i+1]
		}
	}
	return result, nil
}

// YAML permits numeric separators, radix-prefixed integers and leading/trailing
// decimal points. Normalize spelling without passing through binary floating
// point; compare numeric value rather than requiring JSON spelling in YAML.
func sourceJSONNumber(node *yaml.Node) (json.RawMessage, error) {
	raw := strings.ReplaceAll(node.Value, "_", "")
	if len(raw) > 4096 {
		return nil, fmt.Errorf("number requires exact editor JSON support before conversion")
	}
	raw = strings.TrimPrefix(raw, "+")
	if node.ShortTag() == "!!int" {
		value, ok := new(big.Int).SetString(raw, 0)
		if !ok {
			value, ok = new(big.Int).SetString(raw, 10)
		}
		if !ok {
			return nil, fmt.Errorf("invalid source integer")
		}
		raw = value.String()
	} else {
		parts := strings.SplitN(strings.ToLower(raw), "e", 2)
		mantissa := parts[0]
		if strings.HasPrefix(mantissa, ".") {
			mantissa = "0" + mantissa
		}
		if strings.HasPrefix(mantissa, "-.") {
			mantissa = "-0" + mantissa[1:]
		}
		if strings.HasSuffix(mantissa, ".") {
			mantissa += "0"
		}
		raw = mantissa
		if len(parts) == 2 {
			raw += "e" + parts[1]
		}
	}
	if !json.Valid([]byte(raw)) {
		return nil, fmt.Errorf("number requires exact editor JSON support before conversion")
	}
	return json.RawMessage(raw), nil
}
