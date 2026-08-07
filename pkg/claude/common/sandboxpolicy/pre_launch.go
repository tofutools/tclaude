package sandboxpolicy

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// MaxPreLaunchBlocks and MaxPreLaunchScriptBytes are deliberately generous.
	// They exist to stop an absurd payload from reaching the launch path, not
	// to express a policy about what an operator may write; tightening them (and
	// deciding whether the character set wants narrowing) is TCL-1043's call,
	// after the feature has been used.
	MaxPreLaunchBlocks      = 32
	MaxPreLaunchScriptBytes = 64 * 1024
	MaxPreLaunchBlockName   = 128
	MaxPreLaunchExports     = 64
)

// PreLaunchBlock is one operator-authored shell fragment run inside the
// sandbox, after the profile's environment is exported and before the harness
// binary starts.
//
// It exists for setup that the declarative fields cannot express: a value that
// must be computed per launch, a directory layout a tool insists on, a wrapper
// dropped earlier on PATH. The declarative fields remain the better answer
// whenever they suffice — `environment` for a fixed value, `agent_directories`
// for a private writable directory injected as a variable — because those can
// be inspected, composed and disclosed, while a script can only be run.
//
// A block runs after the sandbox is established, with authority the launch has
// already checked, so it holds nothing the agent does not already have. That is
// what makes it safe to compose freely and why blocks take no part in lineage
// containment. The confinement itself is only as strong as the sandbox in force
// — under a harness-native sandbox, or none at all, the pane is unconfined and
// so is the block; authoring profiles is gated on sandbox-profiles.manage.
type PreLaunchBlock struct {
	Name   string `json:"name"`
	Script string `json:"script"`
	// Exports names the environment variables this block publishes. It is
	// OPTIONAL and never enforced: Claude Code, Copilot and OpenCode inherit the
	// pane's environment, so a block works there whether or not it declares
	// anything. It exists because Codex scrubs the environment of its shell
	// tool, so the values a block produces must be forwarded by name — that
	// forwarding is not built yet (TCL-1040) — and because a declared name is
	// something the dashboard can show.
	//
	// Unlike a profile's `environment` entries, these names are NOT checked
	// against the reserved list. Reaching a name the declarative field refuses
	// (XDG_CONFIG_HOME, PATH) is a large part of why this feature exists.
	Exports []string `json:"exports,omitempty"`
}

// preLaunchNameRE keeps block names to something a human reads in an editor
// row and a launch-script comment: a leading alphanumeric, then word-ish
// characters. Names are identity for include composition, so they must not
// carry whitespace or shell metacharacters.
var preLaunchNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func normalizePreLaunch(in []PreLaunchBlock) ([]PreLaunchBlock, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxPreLaunchBlocks {
		return nil, fmt.Errorf("pre_launch has too many blocks (maximum %d)", MaxPreLaunchBlocks)
	}
	seen := make(map[string]struct{}, len(in))
	// Authored order is preserved rather than sorted: these are statements
	// executed in sequence, so order is meaning, not presentation. This is the
	// same reason Includes keeps its authored order.
	out := make([]PreLaunchBlock, 0, len(in))
	for i, block := range in {
		name := strings.TrimSpace(block.Name)
		if name == "" {
			return nil, fmt.Errorf("pre_launch[%d] needs a name", i)
		}
		if len(name) > MaxPreLaunchBlockName || !preLaunchNameRE.MatchString(name) {
			return nil, fmt.Errorf(
				"pre_launch[%d] name %q is invalid (want an alphanumeric-led word up to %d bytes)",
				i, name, MaxPreLaunchBlockName)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("pre_launch has duplicate block name %q", name)
		}
		seen[name] = struct{}{}

		script := block.Script
		if strings.TrimSpace(script) == "" {
			return nil, fmt.Errorf("pre_launch block %q has an empty script", name)
		}
		if len(script) > MaxPreLaunchScriptBytes {
			return nil, fmt.Errorf(
				"pre_launch block %q script is %d bytes (maximum %d)",
				name, len(script), MaxPreLaunchScriptBytes)
		}
		if !utf8.ValidString(script) {
			return nil, fmt.Errorf("pre_launch block %q script is not valid UTF-8", name)
		}
		// A NUL cannot survive the trip through an argv/command string at all,
		// so it would be silently truncated somewhere downstream rather than
		// running as written. Everything else, including whatever whitespace
		// the operator likes, is theirs to write.
		if strings.ContainsRune(script, 0) {
			return nil, fmt.Errorf("pre_launch block %q script contains a NUL byte", name)
		}

		exports, err := normalizePreLaunchExports(name, block.Exports)
		if err != nil {
			return nil, err
		}
		out = append(out, PreLaunchBlock{Name: name, Script: script, Exports: exports})
	}
	return out, nil
}

func normalizePreLaunchExports(block string, in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxPreLaunchExports {
		return nil, fmt.Errorf(
			"pre_launch block %q declares too many exports (maximum %d)", block, MaxPreLaunchExports)
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, name := range in {
		name = strings.TrimSpace(name)
		if len(name) > MaxEnvironmentName || !environmentNameRE.MatchString(name) {
			return nil, fmt.Errorf(
				"pre_launch block %q export %q is not a valid environment-variable name", block, name)
		}
		// Deliberately NOT checked against isReservedEnvironmentName: a block
		// reaching XDG_CONFIG_HOME or PATH is the point of the feature.
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	// Exports are a set, not a sequence, so canonical order is stable order.
	sort.Strings(out)
	return out, nil
}

// clonePreLaunch deep-copies blocks so a caller's later mutation cannot reach
// a normalized profile or a frozen snapshot.
func clonePreLaunch(in []PreLaunchBlock) []PreLaunchBlock {
	if len(in) == 0 {
		return nil
	}
	out := make([]PreLaunchBlock, 0, len(in))
	for _, block := range in {
		copied := block
		if len(block.Exports) > 0 {
			copied.Exports = append([]string(nil), block.Exports...)
		}
		out = append(out, copied)
	}
	return out
}

// mergePreLaunch layers one tier's blocks onto an accumulated list.
//
// A block whose name the accumulator already carries REPLACES it in place,
// keeping its original position: an including profile overriding one block must
// not silently reorder the others relative to it, because these are sequential
// statements. A block with a new name appends. This is the same
// "later layer wins for the same key, distinct keys coexist" rule the other
// composed fields use, with position added because order is meaning here.
func mergePreLaunch(accumulated, incoming []PreLaunchBlock) []PreLaunchBlock {
	if len(incoming) == 0 {
		return accumulated
	}
	out := clonePreLaunch(accumulated)
	index := make(map[string]int, len(out))
	for i, block := range out {
		index[block.Name] = i
	}
	for _, block := range clonePreLaunch(incoming) {
		if at, exists := index[block.Name]; exists {
			out[at] = block
			continue
		}
		index[block.Name] = len(out)
		out = append(out, block)
	}
	return out
}

// PreLaunchExports is the union of every declared export name across blocks, in
// stable order.
//
// A declared export needs no per-harness forwarding machinery. A block runs in
// the launching shell, so its values are in the harness process's own
// environment, and all four harnesses pass that down to the commands they run.
// Codex was the doubtful one — it can rebuild a tool command's environment from
// `shell_environment_policy` — but that policy's default is `inherit = "all"`,
// verified against codex-cli 0.146.1: a value a block exports is visible to the
// shell tool unmodified, while `exclude`/`inherit = "core"` do strip it, which
// is what proves the policy is applied on that path rather than bypassed.
//
// So the remaining gap is only an operator config.toml that narrows `inherit`
// or excludes the name. Closing it needs the value at command-build time, which
// a block-computed value by definition does not have; it would take rendering
// `-c shell_environment_policy.set.<name>=<value>` from the pane shell after the
// blocks run. That is TCL-1040, deliberately deferred as hardening.
//
// What the declaration buys today is the launch-time check in the rendered
// fragment: a block that finishes without defining a name it promised stops the
// launch there, instead of the agent starting subtly misconfigured.
func PreLaunchExports(blocks []PreLaunchBlock) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, block := range blocks {
		for _, name := range block.Exports {
			if _, exists := seen[name]; exists {
				continue
			}
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
