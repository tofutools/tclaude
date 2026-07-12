package sandboxpolicy

import (
	"fmt"
	"maps"
	"sort"
)

// LookupProfile resolves an included profile by its exact registry name.
// Returning (nil, nil) means the profile does not exist; Flatten converts
// that into a fail-closed error naming the dangling reference.
type LookupProfile func(name string) (*Profile, error)

// Flatten expands a profile's includes recursively into a single
// self-contained profile with no remaining Includes. Included profiles apply
// first, in listed order, then the including profile's own entries: for an
// exact canonical filesystem path or environment name that appears in several
// layers, the later layer wins. Overlapping-but-distinct paths (an ancestor in
// one layer, a descendant in another) are both kept, exactly as if they had
// been authored in one profile.
//
// Layering here is an authoring convenience inside a single operator-owned
// registry — the author could equally have inlined the entries — so a local
// grant may override an included deny on the same path. Cross-scope
// resolution (global → group → explicit) keeps its deny-dominates union in
// Resolve; Flatten runs before it, once per scope.
//
// Every visited profile is normalized, so the merged keys are canonical.
// Validation runs as its own exact pass before any merging: missing
// references and cycles fail closed, and the longest include-edge chain is
// capped at MaxIncludeDepth — the same unit and bound the registry write
// paths enforce, and independent of include order or traversal history. The
// merge itself memoizes each distinct profile, so diamond-shaped graphs cost
// linear work.
func Flatten(in Profile, lookup LookupProfile) (Profile, error) {
	// Persistence normalization, matching Resolve: missing read/write paths
	// flow through included profiles into the frozen snapshot (launch filters
	// them until they exist), while protected-root and deny rules keep their
	// strict checks.
	root, _, err := NormalizeForPersistence(in)
	if err != nil {
		return Profile{}, err
	}
	if len(root.Includes) == 0 {
		return root, nil
	}
	if lookup == nil {
		return Profile{}, fmt.Errorf("sandbox profile %q has includes but no registry lookup was provided", root.Name)
	}
	f := &flattener{
		lookup:   lookup,
		profiles: map[string]Profile{root.Name: root},
		depths:   map[string]int{},
		onPath:   map[string]bool{root.Name: true},
		memo:     map[string]*flattenedParts{},
	}
	// Exact validation pass: every reachable profile is loaded and normalized
	// once, cycles are detected, and the root's longest include-edge chain is
	// bounded. Only after the graph is proven well-formed does the memoized
	// merge run, so memo reuse can never skip a limit check.
	rootDepth := 0
	for _, name := range root.Includes {
		d, err := f.chainDepth(name)
		if err != nil {
			return Profile{}, err
		}
		rootDepth = max(rootDepth, d+1)
	}
	if rootDepth > MaxIncludeDepth {
		return Profile{}, fmt.Errorf("sandbox profile %q nests includes deeper than %d levels", root.Name, MaxIncludeDepth)
	}
	parts := f.compose(root)
	out := Profile{
		Name:        root.Name,
		Filesystem:  make([]FilesystemGrant, 0, len(parts.filesystem)),
		Environment: make([]EnvironmentEntry, 0, len(parts.environment)),
	}
	for _, grant := range parts.filesystem {
		out.Filesystem = append(out.Filesystem, grant)
	}
	for _, entry := range parts.environment {
		out.Environment = append(out.Environment, entry)
	}
	sort.Slice(out.Filesystem, func(i, j int) bool { return out.Filesystem[i].Path < out.Filesystem[j].Path })
	sort.Slice(out.Environment, func(i, j int) bool { return out.Environment[i].Name < out.Environment[j].Name })
	return out, nil
}

type flattenedParts struct {
	filesystem  map[string]FilesystemGrant
	environment map[string]EnvironmentEntry
}

type flattener struct {
	lookup   LookupProfile
	profiles map[string]Profile // loaded and normalized once per name
	depths   map[string]int     // longest include-edge chain below each name
	onPath   map[string]bool
	memo     map[string]*flattenedParts
}

// load resolves and normalizes one profile exactly once.
func (f *flattener) load(name string) (Profile, error) {
	if p, done := f.profiles[name]; done {
		return p, nil
	}
	profile, err := f.lookup(name)
	if err != nil {
		return Profile{}, fmt.Errorf("load included sandbox profile %q: %w", name, err)
	}
	if profile == nil {
		return Profile{}, fmt.Errorf("included sandbox profile %q was not found", name)
	}
	normalized, _, err := NormalizeForPersistence(*profile)
	if err != nil {
		return Profile{}, fmt.Errorf("normalize included sandbox profile %q: %w", name, err)
	}
	f.profiles[name] = normalized
	return normalized, nil
}

// chainDepth returns the longest include-edge chain below name (0 for a
// profile with no includes), detecting cycles exactly: the depth memo admits
// only completed — hence acyclic — profiles, so every node on a cycle is
// still on the recursion path when revisited.
func (f *flattener) chainDepth(name string) (int, error) {
	if d, done := f.depths[name]; done {
		return d, nil
	}
	if f.onPath[name] {
		return 0, fmt.Errorf("sandbox profile include cycle through %q", name)
	}
	p, err := f.load(name)
	if err != nil {
		return 0, err
	}
	f.onPath[name] = true
	deepest := 0
	for _, include := range p.Includes {
		d, err := f.chainDepth(include)
		if err != nil {
			return 0, err
		}
		deepest = max(deepest, d+1)
	}
	delete(f.onPath, name)
	f.depths[name] = deepest
	return deepest, nil
}

// compose builds a validated profile's flattened parts: its includes in
// order, then its own entries, with the later layer winning per exact key.
// The validation pass has already loaded every reachable profile and proven
// the graph acyclic and depth-bounded, so this is a pure memoized merge.
func (f *flattener) compose(p Profile) *flattenedParts {
	out := &flattenedParts{
		filesystem:  map[string]FilesystemGrant{},
		environment: map[string]EnvironmentEntry{},
	}
	for _, name := range p.Includes {
		parts, done := f.memo[name]
		if !done {
			parts = f.compose(f.profiles[name])
			f.memo[name] = parts
		}
		maps.Copy(out.filesystem, parts.filesystem)
		maps.Copy(out.environment, parts.environment)
	}
	for _, grant := range p.Filesystem {
		out.filesystem[grant.Path] = grant
	}
	for _, entry := range p.Environment {
		out.environment[entry.Name] = entry
	}
	return out
}
