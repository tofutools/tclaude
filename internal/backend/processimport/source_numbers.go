package processimport

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/processimport/legacy"
	"gopkg.in/yaml.v3"
)

// Check source tokens before the retained YAML decoder's interface{} values can
// hide a loss of decimal precision. Inspection still preserves the full source.
func validateSourceNumbers(source string, template *legacy.Template) error {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(source), &root); err != nil {
		return err
	}
	if len(root.Content) != 1 {
		return fmt.Errorf("one source document required")
	}
	params, err := sourceMapValue(root.Content[0], "params")
	if err != nil {
		return err
	}
	visited := 0
	var walk func(*yaml.Node, int) error
	walk = func(node *yaml.Node, depth int) error {
		if node == nil {
			return nil
		}
		visited++
		if visited > 100000 || depth > 128 {
			return fmt.Errorf("parameter source expansion exceeds conversion bounds")
		}
		if node.Kind == yaml.AliasNode {
			return walk(node.Alias, depth+1)
		}
		if node.Kind == yaml.ScalarNode && (node.ShortTag() == "!!int" || node.ShortTag() == "!!float") {
			raw, err := sourceJSONNumber(node)
			if err != nil {
				return err
			}
			return exactEditorNumbers(raw)
		}
		for _, child := range node.Content {
			if err := walk(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	for key, parameter := range template.Params {
		row, err := sourceMapValue(params, key)
		if err != nil {
			return err
		}
		value, err := sourceMapValue(row, "default")
		if err != nil {
			return err
		}
		if value == nil && parameter.Default != nil {
			return fmt.Errorf("parameter %s/default: cannot resolve retained source default", key)
		}
		if err := walk(value, 0); err != nil {
			return fmt.Errorf("parameter %s/default: %w", key, err)
		}
	}

	return nil
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
