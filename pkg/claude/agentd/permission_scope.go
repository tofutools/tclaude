package agentd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// permissionScopeMaxJSONBytes is shared conceptually with the v195 column
// CHECKs. Keep the values in lockstep: the HTTP writer rejects anything the
// database would reject, with a useful typed error instead of a SQLite error.
const permissionScopeMaxJSONBytes = 262144

// ScopeDim is one typed action attribute a permission grant may constrain.
// Registry entries advertise these string values over /v1/permissions/slugs.
type ScopeDim string

const (
	ScopeDimGroup           ScopeDim = "group"
	ScopeDimSpawnProfile    ScopeDim = "spawn_profile"
	ScopeDimProcessTemplate ScopeDim = "process_template"
	ScopeDimRemote          ScopeDim = "remote"
	ScopeDimTargetAgent     ScopeDim = "target_agent"
)

// PermissionScope is the persisted scope shape: dimensions AND together;
// matchers within one dimension OR together. Phase 1 validates and renders
// this value but deliberately does not evaluate it at authorization gates.
type PermissionScope map[ScopeDim][]string

type permissionScopeMatcherKind uint8

const (
	permissionScopeMatchExact permissionScopeMatcherKind = iota
	permissionScopeMatchRemotePattern
)

type permissionScopeDimension struct {
	selectors map[string]struct{}
	matcher   permissionScopeMatcherKind
}

// permissionScopeDimensions is the closed dimension registry. Besides the
// selectors accepted by a dimension, it declares how literal matchers are
// evaluated. Most dimensions are exact; remote deliberately reuses the git
// proxy's slash-segmented pattern language.
var permissionScopeDimensions = map[ScopeDim]permissionScopeDimension{
	ScopeDimGroup:           {},
	ScopeDimSpawnProfile:    {},
	ScopeDimProcessTemplate: {},
	ScopeDimRemote:          {matcher: permissionScopeMatchRemotePattern},
	ScopeDimTargetAgent: {selectors: map[string]struct{}{
		"@descendants":  {},
		"@self-spawned": {},
	}},
}

// parsePermissionScope parses, validates and canonicalizes the optional wire
// value. Empty, null, and {} all mean an unscoped grant and persist as "" so
// existing rows and readers retain their exact historical representation.
func parsePermissionScope(raw json.RawMessage) (PermissionScope, string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, "", nil
	}
	if len(trimmed) > permissionScopeMaxJSONBytes {
		return nil, "", fmt.Errorf("permission scope exceeds %d bytes", permissionScopeMaxJSONBytes)
	}
	if trimmed[0] != '{' {
		return nil, "", fmt.Errorf("permission scope must be a JSON object")
	}
	var wire map[string][]string
	if err := json.Unmarshal(trimmed, &wire); err != nil {
		return nil, "", fmt.Errorf("invalid permission scope: %w", err)
	}
	if wire == nil {
		return nil, "", fmt.Errorf("permission scope must be a JSON object")
	}
	scope := make(PermissionScope, len(wire))
	for rawDim, rawMatchers := range wire {
		dim := ScopeDim(strings.TrimSpace(rawDim))
		if dim == "" {
			return nil, "", fmt.Errorf("permission scope dimension must not be empty")
		}
		if string(dim) != rawDim {
			return nil, "", fmt.Errorf("permission scope dimension %q must not contain surrounding whitespace", rawDim)
		}
		dimension, knownDim := permissionScopeDimensions[dim]
		if !knownDim {
			return nil, "", fmt.Errorf("unknown permission scope dimension %q", rawDim)
		}
		if len(rawMatchers) == 0 {
			return nil, "", fmt.Errorf("permission scope dimension %q must have at least one matcher", dim)
		}
		seen := map[string]bool{}
		for _, matcher := range rawMatchers {
			if strings.TrimSpace(matcher) == "" {
				return nil, "", fmt.Errorf("permission scope dimension %q contains an empty matcher", dim)
			}
			if strings.IndexFunc(matcher, unicode.IsControl) >= 0 {
				return nil, "", fmt.Errorf("permission scope dimension %q contains a matcher with control characters", dim)
			}
			if strings.HasPrefix(matcher, "@") {
				if _, ok := dimension.selectors[matcher]; !ok {
					return nil, "", fmt.Errorf("unknown selector %q for permission scope dimension %q", matcher, dim)
				}
			}
			if !seen[matcher] {
				scope[dim] = append(scope[dim], matcher)
				seen[matcher] = true
			}
		}
		sort.Strings(scope[dim])
	}
	if len(scope) == 0 {
		return nil, "", nil
	}
	canonical, err := json.Marshal(scope)
	if err != nil {
		return nil, "", fmt.Errorf("encode permission scope: %w", err)
	}
	if len(canonical) > permissionScopeMaxJSONBytes {
		return nil, "", fmt.Errorf("permission scope exceeds %d bytes", permissionScopeMaxJSONBytes)
	}
	return scope, string(canonical), nil
}

func canonicalPermissionScopeForSlug(slug, raw string) (string, error) {
	scope, canonical, err := parsePermissionScope(json.RawMessage(raw))
	if err != nil {
		return "", err
	}
	if err := validatePermissionScopeForSlug(slug, scope); err != nil {
		return "", err
	}
	return canonical, nil
}

// validatePermissionScopeForSlug rejects dimensions that are meaningful in
// general but not declared for this slug. Unknown slugs remain subject to the
// existing endpoint rule; an empty scope does not add a new rejection path.
func validatePermissionScopeForSlug(slug string, scope PermissionScope) error {
	if len(scope) == 0 {
		return nil
	}
	var declared map[ScopeDim]bool
	for _, p := range permissionRegistry {
		if p.Slug != slug {
			continue
		}
		declared = map[ScopeDim]bool{}
		for _, dim := range p.ScopeDims {
			declared[dim] = true
		}
		break
	}
	if declared == nil {
		return fmt.Errorf("unknown permission slug %q", slug)
	}
	for dim := range scope {
		if !declared[dim] {
			return fmt.Errorf("permission slug %q does not declare scope dimension %q", slug, dim)
		}
	}
	return nil
}

// permissionScopeDisplay is the deterministic one-line provenance form.
func permissionScopeDisplay(scope PermissionScope) string {
	if len(scope) == 0 {
		return ""
	}
	dims := make([]string, 0, len(scope))
	for dim := range scope {
		dims = append(dims, string(dim))
	}
	sort.Strings(dims)
	parts := make([]string, 0, len(dims))
	for _, dim := range dims {
		parts = append(parts, dim+"="+strings.Join(scope[ScopeDim(dim)], ","))
	}
	return strings.Join(parts, " ")
}

func appendUnique(out []string, seen map[string]bool, s string) []string {
	if s == "" || seen[s] {
		return out
	}
	seen[s] = true
	return append(out, s)
}

// scopeDimOptionsSnapshot builds the permission editor's dimension pickers.
//
// This is the ONE place per-dimension knowledge lives on the read path: the
// dimension set itself comes from the registry, and each case only answers
// "what are the choosable values". A dimension with no case still ships — with
// its selectors and an empty value list — so the editor offers it as free
// text rather than silently hiding a dimension the gate will happily enforce.
//
// Only catalogues the snapshot ALREADY loaded are used. process_template lives
// in a filesystem store whose listing is deliberately kept off the 2s poll
// (see handleProcessTemplates), and target_agent has no meaningful fixed list
// at all; both are free text plus their selectors, which is exactly what the
// CLI's --scope accepts.
func scopeDimOptionsSnapshot(groups []*db.AgentGroup, profiles []spawnProfileJSON) map[ScopeDim]snapshotScopeDimOptions {
	out := make(map[ScopeDim]snapshotScopeDimOptions, len(permissionScopeDimensions))
	for _, dim := range permissionScopeDims() {
		options := snapshotScopeDimOptions{Selectors: permissionScopeSelectorsFor(dim)}
		switch dim {
		case ScopeDimGroup:
			for _, g := range groups {
				// An archived group grants nothing, so offering it as a scope
				// value would only invite an operator to write a dead grant.
				if g == nil || g.IsArchived() {
					continue
				}
				options.Values = append(options.Values, g.Name)
			}
		case ScopeDimSpawnProfile:
			for _, p := range profiles {
				options.Values = append(options.Values, p.Name)
			}
		}
		sort.Strings(options.Values)
		out[dim] = options
	}
	return out
}

// permissionScopeDims returns every dimension the daemon knows, sorted. It
// reads the closed dimension registry rather than a second hand-kept list, so
// a dimension added there is offered by the editor on the same commit.
func permissionScopeDims() []ScopeDim {
	dims := make([]ScopeDim, 0, len(permissionScopeDimensions))
	for dim := range permissionScopeDimensions {
		dims = append(dims, dim)
	}
	sort.Slice(dims, func(i, j int) bool { return dims[i] < dims[j] })
	return dims
}

// permissionScopeSelectorsFor returns the sorted @selectors a dimension
// accepts, or nil for a dimension with none.
func permissionScopeSelectorsFor(dim ScopeDim) []string {
	selectors := permissionScopeDimensions[dim].selectors
	if len(selectors) == 0 {
		return nil
	}
	out := make([]string, 0, len(selectors))
	for selector := range selectors {
		out = append(out, selector)
	}
	sort.Strings(out)
	return out
}

func permissionScopeFromJSON(raw string) PermissionScope {
	if raw == "" {
		return nil
	}
	scope, _, err := parsePermissionScope(json.RawMessage(raw))
	if err != nil {
		return nil
	}
	return scope
}

// permissionProvenance appends the winning persisted scopes without changing
// the resolver's source or decision. Multiple group grants are rendered as an
// OR of their scopes; one unscoped row makes that tier unscoped.
func permissionProvenance(source permSource, rawScopes []string) string {
	base := string(source)
	if len(rawScopes) == 0 {
		return base
	}
	seen := map[string]bool{}
	var rendered []string
	for _, raw := range rawScopes {
		scope, err := permissionScopeForEval(raw)
		if err != nil {
			// The gate refuses a row it cannot decode (evalPermissionScope).
			// Rendering it as absence would show the grant as UNSCOPED —
			// the widest possible reading of the one row that authorizes
			// nothing. Say so instead, so the listing and the gate agree.
			rendered = appendUnique(rendered, seen, "unreadable scope")
			continue
		}
		if len(scope) == 0 {
			// An unscoped row in the tier makes the whole tier unscoped.
			return base
		}
		rendered = appendUnique(rendered, seen, permissionScopeDisplay(scope))
	}
	if len(rendered) == 0 {
		return base
	}
	sort.Strings(rendered)
	parts := make([]string, len(rendered))
	for i, display := range rendered {
		parts[i] = "[" + display + "]"
	}
	return base + " " + strings.Join(parts, " OR ")
}
