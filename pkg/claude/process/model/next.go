package model

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

const DefaultOutcome = "next"

type Next map[string]string

func (n *Next) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var target string
		if err := value.Decode(&target); err != nil {
			return err
		}
		if target == "" {
			*n = nil
			return nil
		}
		*n = Next{DefaultOutcome: target}
		return nil
	case yaml.MappingNode:
		out := make(Next, len(value.Content)/2)
		for i := 0; i < len(value.Content); i += 2 {
			key := value.Content[i].Value
			var target string
			if err := value.Content[i+1].Decode(&target); err != nil {
				return fmt.Errorf("next.%s: %w", key, err)
			}
			out[key] = target
		}
		*n = out
		return nil
	case yaml.SequenceNode:
		return fmt.Errorf("next must be a target string or outcome map")
	case yaml.AliasNode, yaml.DocumentNode:
		return fmt.Errorf("next must be a target string or outcome map")
	default:
		*n = nil
		return nil
	}
}

func (n Next) MarshalYAML() (any, error) {
	if len(n) == 1 {
		if target, ok := n[DefaultOutcome]; ok {
			return target, nil
		}
	}
	return map[string]string(n), nil
}
