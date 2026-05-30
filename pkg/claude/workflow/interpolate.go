package workflow

// interpolate.go resolves {{...}} references in a node's prompt / run command
// against an instance's variable scope (params + captured outputs). It is pure
// and storage-agnostic: the engine (agentd) assembles the Scope from the
// instance's params JSON and vars JSON and calls Interpolate; nothing here
// touches a DB.
//
// Two resolution surfaces share one syntax:
//   - text interpolation (Interpolate) — substitute into a prompt or shell
//     command, where the result must be a string. A list/map value is rendered
//     compactly (JSON) so it still produces *something* usable in a command.
//   - value resolution (Resolve) — fetch the raw typed value behind a single
//     reference, preserving string/list/map, for capture-to-capture data flow.

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// refPattern matches a single {{ ref }} placeholder. The ref is a dotted path:
// a leading segment (a param or capture name) plus optional `.field` segments
// (e.g. `plan.output`, `config.region`). Whitespace inside the braces is
// trimmed. Segments are word-ish (letters, digits, underscore, hyphen) so a
// stray `{{` in prose without a valid ref is left untouched.
var refPattern = regexp.MustCompile(`\{\{\s*([\w-]+(?:\.[\w-]+)*)\s*\}\}`)

// Scope is the variable environment a node interpolates against: top-level keys
// are param names and capture names; values are the decoded JSON (string,
// float64, bool, []any, map[string]any, or nil — whatever encoding/json yields).
// A dotted ref descends into nested maps.
type Scope map[string]any

// Resolve returns the raw typed value behind a dotted reference, and whether it
// was found. `plan` returns the whole capture; `plan.output` descends one level;
// a miss (unknown head, descent into a non-map, missing field) returns
// (nil, false). Type is preserved — a list stays a []any, a map stays a map.
func (s Scope) Resolve(ref string) (any, bool) {
	parts := strings.Split(ref, ".")
	var cur any = map[string]any(s)
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := m[p]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// Interpolate substitutes every {{ ref }} in text with its resolved value
// rendered as a string. A missing ref is left verbatim (so a typo is visible in
// the output rather than silently blanked) and its name is reported in the
// returned `missing` slice (sorted, deduped) so the engine can decide whether to
// fail the node. A scalar renders naturally (string as-is, number/bool via JSON);
// a list/map renders as compact JSON.
func (s Scope) Interpolate(text string) (out string, missing []string) {
	missSet := map[string]bool{}
	out = refPattern.ReplaceAllStringFunc(text, func(match string) string {
		ref := strings.TrimSpace(refPattern.FindStringSubmatch(match)[1])
		v, ok := s.Resolve(ref)
		if !ok {
			missSet[ref] = true
			return match // leave the placeholder so the gap is visible
		}
		return renderValue(v)
	})
	if len(missSet) > 0 {
		missing = sortedKeys(missSet)
	}
	return out, missing
}

// renderValue turns a resolved value into its string form for text
// interpolation. Strings pass through UNTOUCHED; everything else (numbers,
// bools, lists, maps, nil) is JSON-encoded for an unambiguous representation.
//
// WARNING — NOT shell-escaped. JSON encoding makes a value unambiguous but does
// NOT neutralise shell metacharacters, and a string value is emitted verbatim.
// When the engine interpolates a captured value into a `tool`/`program` node's
// `run` command, that value lands in shell command position with no quoting, so
// a capture containing `; rm -rf …`, backticks, or `$(…)` executes. This is
// acceptable today ONLY because every interpolated value originates from a
// first-party template's own params or a sibling node's output within the same
// instance (the external-source gate keeps third-party templates from
// auto-running at all) — i.e. the trust boundary is the template author, who
// already controls the `run` command directly. It is NOT safe for
// attacker-controlled captures. Proper fix (tracked as a follow-up): quote
// interpolations in shell command position, or refuse to interpolate a capture
// into `run` without explicit opt-in. Prompts (ai nodes) are not shell, so the
// risk there is prompt-injection, not command execution.
func renderValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// sortedKeys returns the keys of a set, sorted, for deterministic output.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
