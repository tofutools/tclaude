package model

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// PermissionScope retains the operator's named constraints. Dimensions are
// conjunctive; matchers within a dimension are alternatives. Names are not
// immutable revision references: subsequent actions describe their current
// group/profile/template names and evaluate the same authored scope.
type PermissionScope map[string][]string

const MaxPermissionScopeBytes = 262144

// PermissionContext describes the action being authorized. Callers supply
// current resolved values, not raw user requests. Missing dimensions do not
// satisfy a constrained grant.
type PermissionContext struct {
	Group, SpawnProfile, SandboxProfile, ProcessTemplate string
	Remote, LinearTeam, AWBWorkspace, TargetAgent        string
}

func (c PermissionContext) Value(dimension string) string {
	switch dimension {
	case "group":
		return c.Group
	case "spawn_profile":
		return c.SpawnProfile
	case "sandbox_profile":
		return c.SandboxProfile
	case "process_template":
		return c.ProcessTemplate
	case "remote":
		return c.Remote
	case "linear_team":
		return c.LinearTeam
	case "awb_workspace":
		return c.AWBWorkspace
	case "target_agent":
		return c.TargetAgent
	default:
		return ""
	}
}

func (s PermissionScope) Normalize() (PermissionScope, error) {
	if len(s) == 0 {
		return nil, nil
	}
	out := make(PermissionScope, len(s))
	for dimension, matchers := range s {
		switch dimension {
		case "group", "spawn_profile", "sandbox_profile", "process_template", "remote", "linear_team", "awb_workspace", "target_agent":
		default:
			return nil, fmt.Errorf("unknown permission scope dimension %q", dimension)
		}
		if len(matchers) == 0 {
			return nil, fmt.Errorf("permission scope dimension %q requires a matcher", dimension)
		}
		for _, matcher := range matchers {
			if strings.TrimSpace(matcher) == "" || strings.IndexFunc(matcher, unicode.IsControl) >= 0 {
				return nil, fmt.Errorf("invalid matcher for permission scope dimension %q", dimension)
			}
			if strings.HasPrefix(matcher, "@") {
				if dimension != "target_agent" || matcher != "@descendants" && matcher != "@self-spawned" {
					return nil, fmt.Errorf("unknown selector %q for permission scope dimension %q", matcher, dimension)
				}
			} else if dimension == "linear_team" || dimension == "awb_workspace" {
				if !permissionScopeKey(dimension, matcher) {
					return nil, fmt.Errorf("invalid whole-key matcher %q for permission scope dimension %q", matcher, dimension)
				}
			}
		}
		out[dimension] = slices.Clone(matchers)
		slices.Sort(out[dimension])
		out[dimension] = slices.Compact(out[dimension])
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(encoded) > MaxPermissionScopeBytes {
		return nil, fmt.Errorf("permission scope exceeds %d bytes", MaxPermissionScopeBytes)
	}
	return out, nil
}

// Matches delegates only relational target selectors to the caller, which must
// resolve them against current durable ancestry. No resolver means no selector
// match. The pure matcher never treats an unknown dimension as unrestricted.
func (s PermissionScope) Matches(c PermissionContext, targetSelector func(selector, target string) bool) bool {
	scope, err := s.Normalize()
	if err != nil {
		return false
	}
	for dimension, matchers := range scope {
		value := c.Value(dimension)
		if value == "" {
			return false
		}
		matched := false
		for _, matcher := range matchers {
			if strings.HasPrefix(matcher, "@") {
				matched = targetSelector != nil && targetSelector(matcher, value)
			} else {
				matched = permissionLiteralMatches(dimension, matcher, value)
			}
			if matched {
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func permissionLiteralMatches(dimension, matcher, value string) bool {
	switch dimension {
	case "linear_team", "awb_workspace":
		return strings.EqualFold(strings.TrimSpace(matcher), strings.TrimSpace(value))
	case "remote":
		// V1 remote patterns match a slash-segment prefix. Only a whole '*' segment
		// is a wildcard; '*.example.com', '**', and partial segments are literals.
		pattern := strings.Split(strings.ToLower(strings.Trim(matcher, "/")), "/")
		target := strings.Split(strings.ToLower(strings.Trim(value, "/")), "/")
		if len(pattern) > len(target) {
			return false
		}
		for i, segment := range pattern {
			if segment != "*" && segment != target[i] {
				return false
			}
		}
		return true
	default:
		return matcher == value
	}
}

func permissionScopeKey(dimension, key string) bool {
	key = strings.TrimSpace(key)
	if len(key) == 0 || len(key) > 16 {
		return false
	}
	if dimension == "awb_workspace" && (key[0] < 'a' || key[0] > 'z') {
		return false
	}
	for _, r := range key {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			continue
		}
		if dimension == "linear_team" && r >= 'A' && r <= 'Z' || dimension == "awb_workspace" && r == '-' {
			continue
		}
		return false
	}
	return true
}
