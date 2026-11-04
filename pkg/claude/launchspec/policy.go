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

// Per-agent relaunch overrides (launch slice 11). A spawn profile, and a
// spawn itself, can set a configurable row for the agents it launches; the
// rows are copied into the agent's launch layers at spawn and override the
// global table for that agent. Precedence per row, highest first: the
// spawn's explicit row, the first spawn-profile tier (named, group default,
// global default) that sets it, the global table, the row's default.

// ModeInherit, on a spawn's explicit row, follows the global table for the
// row even when a spawn profile sets it ("Follow global" in the dashboard).
// It is not a stored mode: the row is simply not overridden.
const ModeInherit RelaunchMode = "inherit"

// ValidateOverrides checks a field → mode map of per-agent rows: each field a
// configurable registry row, each mode one it offers, or inherit when
// allowInherit (a spawn's explicit rows; a profile leaves a row out instead).
func ValidateOverrides(rows map[string]string, allowInherit bool) error {
	for field, mode := range rows {
		f, ok := Lookup(field)
		if !ok {
			return fmt.Errorf("relaunch policy: unknown field %q", field)
		}
		if !f.Configurable {
			return fmt.Errorf("relaunch policy: field %q is %s and cannot be configured", field, f.Layer)
		}
		if allowInherit && RelaunchMode(mode) == ModeInherit {
			continue
		}
		if !slices.Contains(f.RelaunchModes(), RelaunchMode(mode)) {
			want := f.RelaunchModes()
			if allowInherit {
				want = append([]RelaunchMode{ModeInherit}, want...)
			}
			return fmt.Errorf("relaunch policy: field %q: mode %q is not one of %v", field, mode, want)
		}
	}
	return nil
}

// KeepsOldSettings reports whether mode makes a relaunch keep a value over
// the operator's current settings: reuse keeps the stored value, and the
// harness-config row's explicit-wins keeps an explicit posture over the
// re-read sandbox-profile chain. Setting one for an agent, where it differs
// from what the agent would inherit, takes relaunch-policy.manage: an
// operator who tightens a group or profile later would not reach that agent.
// Every other mode applies the operator's current settings and is free.
func KeepsOldSettings(mode RelaunchMode) bool {
	return mode == ModeReuse || mode == ModeExplicitWins
}

// WithOverrides is p with the given rows (field → mode) taking the place of
// its global rows for those fields. Invalid rows are skipped.
func (p RelaunchPolicy) WithOverrides(rows map[string]RelaunchMode) RelaunchPolicy {
	valid := map[string]RelaunchMode{}
	for field, mode := range rows {
		if f, ok := Lookup(field); ok && slices.Contains(f.RelaunchModes(), mode) {
			valid[field] = mode
		}
	}
	if len(valid) == 0 {
		return p
	}
	out := RelaunchPolicy{}
	for _, r := range p.Rows {
		if _, over := valid[r.Field]; !over || r.Scope != ScopeGlobal {
			out.Rows = append(out.Rows, r)
		}
	}
	for _, f := range Fields {
		if mode, ok := valid[f.Key]; ok {
			out.Rows = append(out.Rows, PolicyRow{Scope: ScopeGlobal, Field: f.Key, Mode: mode})
		}
	}
	return out
}
