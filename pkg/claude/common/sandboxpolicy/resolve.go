package sandboxpolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
// ProfileSource is comparable, so callers needing set semantics can key on the
// value directly. It carried IncludedBy/Chain include-attribution fields until
// TCL-791: only break-glass resolution ever populated them, and keeping empty
// fields would advertise an audit trail that no longer exists.
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
	Applied          []ProfileSource            `json:"applied"`
	Filesystem       map[string][]ProfileSource `json:"filesystem"`
	Environment      map[string]ProfileSource   `json:"environment"`
	AgentDirectories map[string][]ProfileSource `json:"agent_directories"`
	Network          *ProfileSource             `json:"network,omitempty"`
	UnixSockets      *ProfileSource             `json:"unix_sockets,omitempty"`
	ResourceMemory   *ProfileSource             `json:"resource_memory,omitempty"`
	ResourceCPU      *ProfileSource             `json:"resource_cpu,omitempty"`
}

// EffectiveProfile is the fully-composed harness-neutral sandbox payload and
// its provenance. Its slices and provenance maps are non-nil even when no
// scope is assigned.
type EffectiveProfile struct {
	Filesystem []FilesystemGrant `json:"filesystem"`
	// MountAliases is derived at resolution time from profile spellings that
	// still contain symlinks. Modern registry profiles retain those spellings
	// in non-authoritative metadata; legacy profiles may have already lost
	// them. Omitempty keeps snapshots with no observable aliases byte-compatible.
	MountAliases     []MountAlias         `json:"mount_aliases,omitempty"`
	Environment      []EnvironmentEntry   `json:"environment"`
	AgentDirectories []string             `json:"agent_directories"`
	NetworkAccess    NetworkAccess        `json:"network_access,omitempty"`
	Network          *NetworkRules        `json:"network,omitempty"`
	UnixSockets      *UnixSocketRules     `json:"unix_sockets,omitempty"`
	ResourceLimits   ResourceLimits       `json:"resource_limits,omitempty"`
	AccessNotices    []AccessNotice       `json:"access_notices,omitempty"`
	Provenance       ResolutionProvenance `json:"provenance"`
}

// resolvedFilesystemGrant is one merged rule. The map it lives in is keyed on
// the GUEST path, so a host directory projected onto two sandbox paths stays
// two rules, while path holds the host authority the revalidation pass and the
// appliers need.
type resolvedFilesystemGrant struct {
	path      string
	mountPath string
	access    Access
	sources   []ProfileSource
}

type observableFilesystemSpelling struct {
	profile  string
	resolved string
	spelling string
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
		Filesystem:       []FilesystemGrant{},
		Environment:      []EnvironmentEntry{},
		AgentDirectories: []string{},
		NetworkAccess:    NetworkAccessInherit,
		Provenance: ResolutionProvenance{
			Applied:          []ProfileSource{},
			Filesystem:       map[string][]ProfileSource{},
			Environment:      map[string]ProfileSource{},
			AgentDirectories: map[string][]ProfileSource{},
		},
	}

	filesystem := map[string]resolvedFilesystemGrant{}
	environment := map[string]string{}
	agentDirectories := map[string][]ProfileSource{}
	networkRules := NetworkRules{}
	unixSocketRules := UnixSocketRules{}
	hasNewNetwork := false
	hasNewUnixSockets := false
	networkListContributors := []string{}
	engineSelections := []NetworkEngineSelection{}
	socketListContributors := []string{}
	observableFilesystemSpellings := []observableFilesystemSpelling{}
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
		// Resolve deliberately has no registry access, so it cannot expand
		// includes itself; accepting one silently would drop its grants.
		if len(normalized.Includes) > 0 {
			return EffectiveProfile{}, fmt.Errorf("%s sandbox profile %q still has unresolved includes at resolution time; flatten it first", tier.scope, normalized.Name)
		}
		if normalized.FilesystemSpellings != nil {
			for _, rule := range normalized.FilesystemSpellings.Rules {
				for _, spelling := range rule.Spellings {
					observableFilesystemSpellings = append(
						observableFilesystemSpellings,
						observableFilesystemSpelling{
							profile:  normalized.Name,
							resolved: rule.ResolvedPath,
							spelling: spelling,
						},
					)
				}
			}
		} else {
			// Raw, non-registry profiles keep TCL-759's pre-existing behavior:
			// their as-passed spellings are still observable. Persisted legacy
			// profiles contain only canonical paths here and therefore derive
			// no aliases.
			for _, grant := range tier.profile.Filesystem {
				canonical, _, canonicalErr := canonicalDirectory(grant.Path, true)
				if canonicalErr != nil {
					return EffectiveProfile{}, fmt.Errorf(
						"discover canonical target for %s sandbox profile %q spelling %q: %w",
						tier.scope, tier.profile.Name, grant.Path, canonicalErr,
					)
				}
				observableFilesystemSpellings = append(
					observableFilesystemSpellings,
					observableFilesystemSpelling{
						profile:  normalized.Name,
						resolved: canonical,
						spelling: grant.Path,
					},
				)
			}
		}
		source := ProfileSource{Scope: tier.scope, Profile: normalized.Name}
		result.Provenance.Applied = append(result.Provenance.Applied, source)
		for _, grant := range normalized.Filesystem {
			guest := grant.GuestPath()
			current, exists := filesystem[guest]
			if !exists {
				filesystem[guest] = resolvedFilesystemGrant{
					path: grant.Path, mountPath: grant.MountPath,
					access: grant.Access, sources: []ProfileSource{source},
				}
				continue
			}
			// Two scopes may legitimately grant the same sandbox path, but only
			// from the same host directory. Disagreeing sources are an authoring
			// conflict the union lattice cannot settle: picking either one would
			// silently discard the other tier's authored grant.
			if current.path != grant.Path {
				return EffectiveProfile{}, fmt.Errorf(
					"sandbox path %q is claimed by two different host paths %q and %q across sandbox profile scopes",
					guest, current.path, grant.Path,
				)
			}
			if accessRank(grant.Access) > accessRank(current.access) {
				current.access = grant.Access
			}
			current.sources = append(current.sources, source)
			filesystem[guest] = current
		}
		for _, entry := range normalized.Environment {
			if _, exists := agentDirectories[entry.Name]; exists {
				return EffectiveProfile{}, fmt.Errorf("environment variable %q is both literal and agent-owned across sandbox profile scopes", entry.Name)
			}
			environment[entry.Name] = entry.Value
			result.Provenance.Environment[entry.Name] = source
		}
		for _, name := range normalized.AgentDirectories {
			if _, exists := environment[name]; exists {
				return EffectiveProfile{}, fmt.Errorf("environment variable %q is both literal and agent-owned across sandbox profile scopes", name)
			}
			agentDirectories[name] = append(agentDirectories[name], source)
		}
		if normalized.NetworkAccess != NetworkAccessInherit {
			result.NetworkAccess = normalized.NetworkAccess
			networkSource := source
			result.Provenance.Network = &networkSource
		}
		if normalized.ResourceLimits.Memory != "" {
			result.ResourceLimits.Memory = normalized.ResourceLimits.Memory
			resourceSource := source
			result.Provenance.ResourceMemory = &resourceSource
		}
		if normalized.ResourceLimits.CPU != nil {
			value := *normalized.ResourceLimits.CPU
			result.ResourceLimits.CPU = &value
			resourceSource := source
			result.Provenance.ResourceCPU = &resourceSource
		}
		axes, err := DeriveAccessAxes(normalized)
		if err != nil {
			return EffectiveProfile{}, fmt.Errorf(
				"derive %s sandbox profile %q access axes: %w",
				tier.scope, normalized.Name, err,
			)
		}
		networkRules = intersectNetworkRules(networkRules, axes.Network)
		unixSocketRules = intersectUnixSocketRules(unixSocketRules, axes.UnixSockets)
		hasNewNetwork = hasNewNetwork || normalized.Network != nil
		hasNewUnixSockets = hasNewUnixSockets || normalized.UnixSockets != nil
		tierLabel := fmt.Sprintf("%s %q", tier.scope, normalized.Name)
		// Engine is composed apart from the access lattice on purpose: it is
		// not an access axis, so there is no strictness to intersect. Each
		// tier's authored opinion is collected here and settled by
		// ResolveNetworkEngine after the loop, which is what produces the
		// which-engine-won-and-from-where disclosure. A tier that named no
		// engine is still recorded, because ResolveNetworkEngine absorbing an
		// unset layer is the precedence rule rather than a caller's omission.
		engineSelections = append(engineSelections, NetworkEngineSelection{
			Layer:  networkEngineLayerForScope(tier.scope),
			Engine: axes.Network.Engine,
			Source: fmt.Sprintf("%s profile %q", tier.scope, normalized.Name),
		})
		if axes.Network.Mode == AccessModeList {
			networkListContributors = appendUniqueStrings(networkListContributors, tierLabel)
			networkSource := source
			result.Provenance.Network = &networkSource
		}
		if axes.UnixSockets.Mode == AccessModeList {
			socketListContributors = appendUniqueStrings(socketListContributors, tierLabel)
			socketSource := source
			result.Provenance.UnixSockets = &socketSource
		}
	}
	if len(networkRules.Deny) > MaxEffectiveNetworkDenyEntries {
		return EffectiveProfile{}, fmt.Errorf(
			"effective network.deny has too many entries (maximum %d)",
			MaxEffectiveNetworkDenyEntries)
	}

	// Re-resolve the already-merged path set. Besides enforcing aggregate
	// invariants, this closes the window in which a path component changes
	// between per-scope normalization and consumption of the result.
	revalidated := map[string]resolvedFilesystemGrant{}
	guestPaths := make([]string, 0, len(filesystem))
	for guest := range filesystem {
		guestPaths = append(guestPaths, guest)
	}
	sort.Strings(guestPaths)
	for _, guest := range guestPaths {
		grant := filesystem[guest]
		normalized, _, err := normalizeFilesystem([]FilesystemGrant{{
			Path: grant.path, Access: grant.access, MountPath: grant.mountPath,
		}}, true)
		if err != nil {
			return EffectiveProfile{}, fmt.Errorf("revalidate effective filesystem path %q: %w", grant.path, err)
		}
		canonical := normalized[0]
		canonicalGuest := canonical.GuestPath()
		current, exists := revalidated[canonicalGuest]
		if !exists {
			revalidated[canonicalGuest] = resolvedFilesystemGrant{
				path: canonical.Path, mountPath: canonical.MountPath,
				access: canonical.Access, sources: append([]ProfileSource(nil), grant.sources...),
			}
			continue
		}
		// Two authored guest paths can collapse onto one canonical guest path
		// only when the host side also canonicalizes to one directory; anything
		// else is the same collision the merge above refuses.
		if current.path != canonical.Path {
			return EffectiveProfile{}, fmt.Errorf(
				"effective sandbox path %q is claimed by two different host paths %q and %q",
				canonicalGuest, current.path, canonical.Path,
			)
		}
		if accessRank(canonical.Access) > accessRank(current.access) {
			current.access = canonical.Access
		}
		current.sources = append(current.sources, grant.sources...)
		revalidated[canonicalGuest] = current
	}
	guestPaths = guestPaths[:0]
	for guest := range revalidated {
		guestPaths = append(guestPaths, guest)
	}
	sort.Strings(guestPaths)
	for _, guest := range guestPaths {
		grant := revalidated[guest]
		result.Filesystem = append(result.Filesystem, FilesystemGrant{
			Path: grant.path, Access: grant.access, MountPath: grant.mountPath,
		})
		// Provenance stays keyed on the host path: it answers "which profile
		// granted authority over this directory", and the host path is the
		// authority-bearing side. A directory mounted twice lists both
		// contributing scopes under its one host key.
		result.Provenance.Filesystem[grant.path] = canonicalSources(
			append(append([]ProfileSource(nil), result.Provenance.Filesystem[grant.path]...),
				grant.sources...))
	}
	// The merged set has to satisfy the same cross-rule mount-path invariants a
	// single profile does; a collision or a denied source can first appear when
	// two scopes compose.
	protectedForMounts, protectedErr := protectedPaths()
	if protectedErr != nil {
		return EffectiveProfile{}, protectedErr
	}
	if err := validateMountPaths(result.Filesystem, protectedForMounts); err != nil {
		return EffectiveProfile{}, fmt.Errorf("validate effective filesystem mount paths: %w", err)
	}
	activeCanonicalPaths := make(map[string]bool, len(result.Filesystem))
	for _, grant := range result.Filesystem {
		activeCanonicalPaths[grant.Path] = true
	}
	aliasesByLink := map[string]MountAlias{}
	for _, observable := range observableFilesystemSpellings {
		if !activeCanonicalPaths[observable.resolved] {
			continue
		}
		aliases, discoveredTarget, err := mountAliasesForPath(observable.spelling)
		if err != nil {
			return EffectiveProfile{}, fmt.Errorf(
				"discover mount aliases for effective filesystem path %q: %w",
				observable.spelling, err,
			)
		}
		// Alias discovery returns the final target from the same component walk
		// that captured alias targets. Comparing that value—not a second walk—
		// prevents a swap-and-restore race from publishing stale routing.
		if err := validateDiscoveredFilesystemSpellingTarget(
			observable.profile, observable.spelling, observable.resolved,
			discoveredTarget,
		); err != nil {
			return EffectiveProfile{}, err
		}
		for _, alias := range aliases {
			if previous, exists := aliasesByLink[alias.Link]; exists && previous.Target != alias.Target {
				return EffectiveProfile{}, fmt.Errorf(
					"mount alias %q resolved to both %q and %q during sandbox-profile resolution",
					alias.Link, previous.Target, alias.Target,
				)
			}
			aliasesByLink[alias.Link] = alias
		}
	}
	for _, alias := range aliasesByLink {
		result.MountAliases = append(result.MountAliases, alias)
	}
	sort.Slice(result.MountAliases, func(i, j int) bool {
		if result.MountAliases[i].Link != result.MountAliases[j].Link {
			return result.MountAliases[i].Link < result.MountAliases[j].Link
		}
		return result.MountAliases[i].Target < result.MountAliases[j].Target
	})

	mergedEnvironment := make([]EnvironmentEntry, 0, len(environment))
	for name, value := range environment {
		mergedEnvironment = append(mergedEnvironment, EnvironmentEntry{Name: name, Value: value})
	}
	if len(mergedEnvironment)+len(agentDirectories) > MaxEnvironmentCount {
		return EffectiveProfile{}, fmt.Errorf("effective environment and agent_directories have too many entries combined (maximum %d)", MaxEnvironmentCount)
	}
	var err error
	result.Environment, err = normalizeEnvironment(mergedEnvironment)
	if err != nil {
		return EffectiveProfile{}, fmt.Errorf("validate effective environment: %w", err)
	}
	for name, sources := range agentDirectories {
		result.AgentDirectories = append(result.AgentDirectories, name)
		result.Provenance.AgentDirectories[name] = canonicalSources(sources)
	}
	sort.Strings(result.AgentDirectories)
	resolvedEngine, err := ResolveNetworkEngine(engineSelections)
	if err != nil {
		return EffectiveProfile{}, fmt.Errorf(
			"resolve network filtering engine across sandbox profile scopes: %w", err)
	}
	networkRules.Engine = resolvedEngine.Engine
	if hasNewNetwork {
		result.Network = cloneNetworkRulesPtr(&networkRules)
		result.NetworkAccess = LegacyNetworkAccessForExport(result.Network, result.NetworkAccess)
	}
	if notice, ok := networkEngineCompositionNotice(resolvedEngine); ok {
		result.AccessNotices = append(result.AccessNotices, notice)
	}
	if hasNewUnixSockets ||
		(hasNewNetwork && unixSocketRules.Mode != AccessModeUnset) {
		result.UnixSockets = cloneUnixSocketRulesPtr(&unixSocketRules)
	}
	if networkRules.Mode == AccessModeList && len(networkRules.Allow) == 0 {
		result.AccessNotices = append(result.AccessNotices,
			compositionNotice("network", networkListContributors))
	}
	if unixSocketRules.Mode == AccessModeList && len(unixSocketRules.Allow) == 0 {
		result.AccessNotices = append(result.AccessNotices,
			compositionNotice("unix_sockets", socketListContributors))
	}
	return result, nil
}

// mountAliasesForPath returns the symlinks a constructed root must recreate
// for one spelling to resolve like the host. Each link points at its fully
// resolved target prefix. Continuing the walk from that target finds a second
// alias when a later path component is itself a symlink.
func mountAliasesForPath(path string) ([]MountAlias, string, error) {
	clean, err := cleanDirectoryPath(path)
	if err != nil {
		return nil, "", err
	}
	volume := filepath.VolumeName(clean)
	root := volume + string(filepath.Separator)
	remaining := strings.TrimPrefix(clean, root)
	if remaining == "" {
		return nil, root, nil
	}
	current := root
	aliases := []MountAlias{}
	components := strings.Split(remaining, string(filepath.Separator))
	for index, component := range components {
		candidate := filepath.Join(current, component)
		info, err := os.Lstat(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				return aliases, filepath.Join(
					append([]string{current}, components[index:]...)...,
				), nil
			}
			return nil, "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			current = candidate
			continue
		}
		target, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return nil, "", err
		}
		target = filepath.Clean(target)
		aliases = append(aliases, MountAlias{
			Link:   filepath.Clean(candidate),
			Target: target,
		})
		current = target
	}
	return aliases, current, nil
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
