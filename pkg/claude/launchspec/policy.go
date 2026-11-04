package launchspec

import (
	"fmt"
	"slices"
)

// RelaunchMode is what a relaunch does with a configurable field.
type RelaunchMode string

const (
	// ModeReResolve reads the current value again (the default for every
	// configurable row).
	ModeReResolve RelaunchMode = "re-resolve"
	// ModeReuse keeps the value stored at the agent's last launch.
	ModeReuse RelaunchMode = "reuse"

	// The harness-config posture row weighs an explicit posture against the
	// one the re-read sandbox-profile chain gives (operator decision Q11).
	// ModeExplicitWins keeps the explicit posture (the default).
	ModeExplicitWins RelaunchMode = "explicit-wins"
	// ModeTightenOnly keeps the explicit posture unless the chain's is
	// narrower (the read-only floor on, or fewer reopened paths).
	ModeTightenOnly RelaunchMode = "tighten-only"
	// ModeChainWins takes the chain's posture.
	ModeChainWins RelaunchMode = "chain-wins"
)

// defaultModes is what a configurable row without its own Modes offers.
var defaultModes = []RelaunchMode{ModeReResolve, ModeReuse}

// RelaunchModes returns the modes a field's policy row can take, the default
// first, or nil for a field the policy cannot set.
func (f Field) RelaunchModes() []RelaunchMode {
	if !f.Configurable {
		return nil
	}
	if len(f.Modes) > 0 {
		return f.Modes
	}
	return defaultModes
}

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

// DefaultRelaunchPolicy is one global row per configurable field, each at
// its default mode.
func DefaultRelaunchPolicy() RelaunchPolicy {
	var p RelaunchPolicy
	for _, f := range Fields {
		if modes := f.RelaunchModes(); len(modes) > 0 {
			p.Rows = append(p.Rows, PolicyRow{Scope: ScopeGlobal, Field: f.Key, Mode: modes[0]})
		}
	}
	return p
}

// Mode returns the mode a relaunch uses for a configurable field: its global
// row, or the field's default. "" for a field the policy cannot set.
func (p RelaunchPolicy) Mode(field string) RelaunchMode {
	f, ok := Lookup(field)
	if !ok {
		return ""
	}
	modes := f.RelaunchModes()
	if len(modes) == 0 {
		return ""
	}
	for _, r := range p.Rows {
		if r.Scope == ScopeGlobal && r.Field == field {
			return r.Mode
		}
	}
	return modes[0]
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
		if !slices.Contains(f.RelaunchModes(), r.Mode) {
			return fmt.Errorf("relaunch policy: field %q: mode %q is not one of %v", r.Field, r.Mode, f.RelaunchModes())
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
// configurable field the policy decides: anything but reuse re-reads (for
// the harness-config posture, the chain is always re-read and Mode says how
// it weighs against an explicit posture). For any other field the registry
// layer decides: group settings, referenced profile contents and live values
// are read again; fixed, spawn-copy and spawn-only values are not. An
// unknown field does not re-resolve.
func (p RelaunchPolicy) ReResolves(field string) bool {
	f, ok := Lookup(field)
	if !ok {
		return false
	}
	if f.Configurable {
		return p.Mode(field) != ModeReuse
	}
	switch f.Layer {
	case LayerGroup, LayerReferenced, LayerLive:
		return true
	}
	return false
}
