package model

import (
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"
	"unicode/utf8"
)

// Environment contains only explicitly authored launch values, never the
// backend's inherited environment. Values are literal, without interpolation.
type Environment map[string]string

func (e Environment) Clone() Environment           { return maps.Clone(e) }
func (e Environment) Equal(other Environment) bool { return maps.Equal(e, other) }

// Validate retains the legacy launch environment limits and reserves the
// replacement providers' credential, isolation and policy control namespace.
func (e Environment) Validate() error {
	if len(e) > 128 {
		return fmt.Errorf("environment has more than 128 entries")
	}
	total := 0
	for name, value := range e {
		if len(name) == 0 || len(name) > 128 {
			return fmt.Errorf("invalid environment name")
		}
		for i, c := range name {
			if c != '_' && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (i == 0 || c < '0' || c > '9') {
				return fmt.Errorf("invalid environment name %q", name)
			}
		}
		if reservedEnvironmentName(name) {
			return fmt.Errorf("environment name %q is reserved for runtime control", name)
		}
		if len(value) > 16384 || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("invalid environment value for %q", name)
		}
		total += len(name) + len(value)
		if total > 65536 {
			return fmt.Errorf("environment exceeds 65536 bytes")
		}
	}
	return nil
}

func reservedEnvironmentName(name string) bool {
	switch name {
	case "HOME", "PATH", "SHELL", "TMPDIR", "TMP", "TEMP", "ENV", "BASH_ENV", "TMUX", "TMUX_PANE", "CLAUDE_CONFIG_DIR":
		return true
	}
	for _, prefix := range []string{"TCLAUDE_", "CLAUDE_CODE_", "CODEX_", "COPILOT_", "OPENCODE_", "XDG_", "LD_", "DYLD_"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// MergeEnvironment copies tiers from lowest to highest precedence and validates
// both each tier and the final result. Empty values explicitly override values.
func MergeEnvironment(tiers ...Environment) (Environment, error) {
	var out Environment
	for _, tier := range tiers {
		if err := tier.Validate(); err != nil {
			return nil, err
		}
		for name, value := range tier {
			if out == nil {
				out = Environment{}
			}
			out[name] = value
		}
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

// Entries returns deterministic argv-independent NAME=value process entries.
func (e Environment) Entries() []string {
	names := make([]string, 0, len(e))
	for name := range e {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]string, 0, len(e))
	for _, name := range names {
		out = append(out, name+"="+e[name])
	}
	return out
}

// Equal compares desired configuration by value, treating absent and empty
// environment identically while keeping every existing scalar field exact.
func (d DesiredConfiguration) Equal(other DesiredConfiguration) bool {
	return SameSandboxSelection(d.HostSandbox, other.HostSandbox) && d.Harness == other.Harness && d.Model == other.Model && d.Effort == other.Effort && d.AutoReview == other.AutoReview && d.AutoMemory == other.AutoMemory && d.PeerMessaging == other.PeerMessaging && d.TrustDirectory == other.TrustDirectory && d.AskUserQuestionTimeout == other.AskUserQuestionTimeout && d.AutoCompactWindow == other.AutoCompactWindow && d.FastMode == other.FastMode && d.ToolGovernance == other.ToolGovernance && d.WorkingDirectory == other.WorkingDirectory && d.Approval == other.Approval && d.Sandbox == other.Sandbox && d.Environment.Equal(other.Environment)
}

// UnmarshalJSON canonicalizes an empty authored set to absence. This preserves
// historical equality and omission semantics in stored configuration revisions.
func (e *Environment) UnmarshalJSON(data []byte) error {
	var decoded map[string]string
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if len(decoded) == 0 {
		decoded = nil
	}
	*e = decoded
	return nil
}

// ValidateEnvironments rejects authored allow-lists that can never match a valid
// launch. Absence remains compatible with the historical empty environment.
func (b ConfigurationBounds) ValidateEnvironments() error {
	if err := b.ValidateHostSandboxProfiles(); err != nil {
		return err
	}
	for _, environment := range b.Environments {
		if err := environment.Validate(); err != nil {
			return err
		}
	}
	return nil
}
