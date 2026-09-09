package model

import (
	"fmt"
	"slices"
)

// ActionScopes constrains individual role grants without widening other actions.
type ActionScopes map[Action]PermissionScope

func (scopes ActionScopes) Normalize(actions []Action) (ActionScopes, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	out := ActionScopes{}
	for action, scope := range scopes {
		if !slices.Contains(actions, action) {
			return nil, fmt.Errorf("constraints reference an action absent from the role: %s", action)
		}
		normalized, err := scope.Normalize()
		if err != nil {
			return nil, err
		}
		if len(normalized) != 0 {
			out[action] = normalized
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
