package launchspec

import "fmt"

// RelaunchMode is what a relaunch does with a configurable field.
type RelaunchMode string

const (
	// ModeReResolve reads the current value again (the default for every
	// configurable row).
	ModeReResolve RelaunchMode = "re-resolve"
	// ModeReuse keeps the value stored at the agent's last launch.
	ModeReuse RelaunchMode = "reuse"
)

// ScopeGlobal is the only policy scope today: one answer per configurable
// row for every agent (operator decision Q9, mock A). The scope key is
// carried in the data so per-group overrides can be added later without
// changing its shape.
const ScopeGlobal = "global"

// PolicyRow is one stored answer: in Scope, Field relaunches with Mode.
type PolicyRow struct {
	Scope string       `json:"scope"`
	Field string       `json:"field"`
	Mode  RelaunchMode `json:"mode"`
}

// RelaunchPolicy is the configurable part of relaunch resolution, as data.
// Rows absent from it take the default, so an empty policy is the default
// policy.
type RelaunchPolicy struct {
	Rows []PolicyRow `json:"rows"`
}

// DefaultRelaunchPolicy is one global row per configurable field, each
// re-resolving.
func DefaultRelaunchPolicy() RelaunchPolicy {
	var p RelaunchPolicy
	for _, f := range Fields {
		if f.Configurable {
			p.Rows = append(p.Rows, PolicyRow{Scope: ScopeGlobal, Field: f.Key, Mode: ModeReResolve})
		}
	}
	return p
}

// Validate rejects a row naming an unknown scope, an unknown or
// non-configurable field, an unknown mode, or the same scope and field twice.
func (p RelaunchPolicy) Validate() error {
	seen := map[[2]string]bool{}
	for _, r := range p.Rows {
		if r.Scope != ScopeGlobal {
			return fmt.Errorf("relaunch policy: unknown scope %q", r.Scope)
		}
		f, ok := Lookup(r.Field)
		if !ok {
			return fmt.Errorf("relaunch policy: unknown field %q", r.Field)
		}
		if !f.Configurable {
			return fmt.Errorf("relaunch policy: field %q is %s and cannot be configured", r.Field, f.Layer)
		}
		if r.Mode != ModeReResolve && r.Mode != ModeReuse {
			return fmt.Errorf("relaunch policy: field %q: unknown mode %q", r.Field, r.Mode)
		}
		k := [2]string{r.Scope, r.Field}
		if seen[k] {
			return fmt.Errorf("relaunch policy: field %q listed twice in scope %q", r.Field, r.Scope)
		}
		seen[k] = true
	}
	return nil
}

// ReResolves reports whether a relaunch reads field's value again. For a
// configurable field the policy row decides, defaulting to re-resolve. For
// any other field the registry layer decides: group settings, referenced
// profile contents and live values are read again; fixed, spawn-copy and
// spawn-only values are not. An unknown field does not re-resolve.
func (p RelaunchPolicy) ReResolves(field string) bool {
	f, ok := Lookup(field)
	if !ok {
		return false
	}
	if f.Configurable {
		for _, r := range p.Rows {
			if r.Scope == ScopeGlobal && r.Field == field {
				return r.Mode != ModeReuse
			}
		}
		return true
	}
	switch f.Layer {
	case LayerGroup, LayerReferenced, LayerLive:
		return true
	}
	return false
}
