package sandboxpolicy

import (
	"fmt"
	"sort"
)

// Scope identifies one tier in sandbox-profile resolution. Resolution always
// applies these in Global → Group → Explicit order.
type Scope string

const (
	ScopeGlobal   Scope = "global"
	ScopeGroup    Scope = "group"
	ScopeExplicit Scope = "explicit"
)

// ProfileSource identifies the named profile that contributed a value at one
// resolution tier.
type ProfileSource struct {
	Scope   Scope  `json:"scope"`
	Profile string `json:"profile"`
}

// Scopes is the complete harness-neutral input to Resolve. Nil means that tier
// has no assignment. Profiles may be persisted canonical values, but Resolve
// deliberately normalizes them again to catch filesystem changes since save.
type Scopes struct {
	Global   *Profile `json:"global,omitempty"`
	Group    *Profile `json:"group,omitempty"`
	Explicit *Profile `json:"explicit,omitempty"`
}

// ResolutionProvenance explains the effective capability bundle. Filesystem
// lists every scope that supplied a canonical path because the union uses
// deny-dominates-write-dominates-read, while Environment names the single
// last-scope winner.
type ResolutionProvenance struct {
	Applied     []ProfileSource            `json:"applied"`
	Filesystem  map[string][]ProfileSource `json:"filesystem"`
	Environment map[string]ProfileSource   `json:"environment"`
}

// EffectiveProfile is the fully-composed harness-neutral sandbox payload and
// its provenance. Its slices and provenance maps are non-nil even when no
// scope is assigned.
type EffectiveProfile struct {
	Filesystem  []FilesystemGrant    `json:"filesystem"`
	Environment []EnvironmentEntry   `json:"environment"`
	Provenance  ResolutionProvenance `json:"provenance"`
}

type resolvedFilesystemGrant struct {
	access  Access
	sources []ProfileSource
}

// Resolve composes global, group, then explicit profiles. Filesystem grants
// form a canonical directory union where deny dominates write, which dominates
// read, independent of tier. This makes a restrictive profile safe to layer
// over a broader global/group grant. Environment entries use last-scope-wins.
// Every input is normalized at resolution time, and each effective path is
// resolved once more after merge. Missing paths retain the canonical lexical
// form derived from their longest existing ancestor so profiles can apply
// before those directories are created. Existing paths still receive full
// symlink, directory, and protected-root validation.
func Resolve(in Scopes) (EffectiveProfile, error) {
	result := EffectiveProfile{
		Filesystem:  []FilesystemGrant{},
		Environment: []EnvironmentEntry{},
		Provenance: ResolutionProvenance{
			Applied:     []ProfileSource{},
			Filesystem:  map[string][]ProfileSource{},
			Environment: map[string]ProfileSource{},
		},
	}

	filesystem := map[string]resolvedFilesystemGrant{}
	environment := map[string]string{}
	for _, tier := range []struct {
		scope   Scope
		profile *Profile
	}{
		{ScopeGlobal, in.Global},
		{ScopeGroup, in.Group},
		{ScopeExplicit, in.Explicit},
	} {
		if tier.profile == nil {
			continue
		}
		normalized, _, err := NormalizeForPersistence(*tier.profile)
		if err != nil {
			return EffectiveProfile{}, fmt.Errorf("normalize %s sandbox profile %q at resolution time: %w", tier.scope, tier.profile.Name, err)
		}
		source := ProfileSource{Scope: tier.scope, Profile: normalized.Name}
		result.Provenance.Applied = append(result.Provenance.Applied, source)
		for _, grant := range normalized.Filesystem {
			current, exists := filesystem[grant.Path]
			if !exists {
				filesystem[grant.Path] = resolvedFilesystemGrant{access: grant.Access, sources: []ProfileSource{source}}
				continue
			}
			if accessRank(grant.Access) > accessRank(current.access) {
				current.access = grant.Access
			}
			current.sources = append(current.sources, source)
			filesystem[grant.Path] = current
		}
		for _, entry := range normalized.Environment {
			environment[entry.Name] = entry.Value
			result.Provenance.Environment[entry.Name] = source
		}
	}

	// Re-resolve the already-merged path set. Besides enforcing aggregate
	// invariants, this closes the window in which a path component changes
	// between per-scope normalization and consumption of the result.
	revalidated := map[string]resolvedFilesystemGrant{}
	paths := make([]string, 0, len(filesystem))
	for path := range filesystem {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		grant := filesystem[path]
		normalized, _, err := normalizeFilesystem([]FilesystemGrant{{Path: path, Access: grant.access}}, true)
		if err != nil {
			return EffectiveProfile{}, fmt.Errorf("revalidate effective filesystem path %q: %w", path, err)
		}
		canonical := normalized[0]
		current, exists := revalidated[canonical.Path]
		if !exists {
			revalidated[canonical.Path] = resolvedFilesystemGrant{access: canonical.Access, sources: append([]ProfileSource(nil), grant.sources...)}
			continue
		}
		if accessRank(canonical.Access) > accessRank(current.access) {
			current.access = canonical.Access
		}
		current.sources = append(current.sources, grant.sources...)
		revalidated[canonical.Path] = current
	}

	paths = paths[:0]
	for path := range revalidated {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		grant := revalidated[path]
		result.Filesystem = append(result.Filesystem, FilesystemGrant{Path: path, Access: grant.access})
		result.Provenance.Filesystem[path] = canonicalSources(grant.sources)
	}

	mergedEnvironment := make([]EnvironmentEntry, 0, len(environment))
	for name, value := range environment {
		mergedEnvironment = append(mergedEnvironment, EnvironmentEntry{Name: name, Value: value})
	}
	var err error
	result.Environment, err = normalizeEnvironment(mergedEnvironment)
	if err != nil {
		return EffectiveProfile{}, fmt.Errorf("validate effective environment: %w", err)
	}
	return result, nil
}

func canonicalSources(in []ProfileSource) []ProfileSource {
	rank := map[Scope]int{ScopeGlobal: 0, ScopeGroup: 1, ScopeExplicit: 2}
	seen := make(map[ProfileSource]struct{}, len(in))
	out := make([]ProfileSource, 0, len(in))
	for _, source := range in {
		if _, exists := seen[source]; exists {
			continue
		}
		seen[source] = struct{}{}
		out = append(out, source)
	}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Scope] < rank[out[j].Scope] })
	return out
}
