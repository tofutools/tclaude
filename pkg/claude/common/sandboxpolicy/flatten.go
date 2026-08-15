package sandboxpolicy

import (
	"fmt"
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
	profile, _, err := FlattenWithNotices(in, lookup)
	return profile, err
}

// FlattenWithNotices is Flatten plus authoring-time access-composition
// diagnostics. Notices are deliberately returned beside Profile, like
// NormalizeForPersistence's missing-path list, so transient diagnostics never
// leak into persisted or exported profile values.
func FlattenWithNotices(in Profile, lookup LookupProfile) (Profile, []AccessNotice, error) {
	// Persistence normalization, matching Resolve: missing read/write paths
	// flow through included profiles into the frozen snapshot (launch filters
	// them until they exist), while protected-root and deny rules keep their
	// strict checks.
	root, _, err := NormalizeForPersistence(in)
	if err != nil {
		return Profile{}, nil, err
	}
	if len(root.Includes) == 0 {
		parts := composeProfileAccessAxes(root)
		return root, accessCompositionNotices(parts), nil
	}
	if lookup == nil {
		return Profile{}, nil, fmt.Errorf("sandbox profile %q has includes but no registry lookup was provided", root.Name)
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
			return Profile{}, nil, err
		}
		rootDepth = max(rootDepth, d+1)
	}
	if rootDepth > MaxIncludeDepth {
		return Profile{}, nil, fmt.Errorf("sandbox profile %q nests includes deeper than %d levels", root.Name, MaxIncludeDepth)
	}
	parts := f.compose(root)
	if len(parts.filesystemConflicts) > 0 {
		return Profile{}, nil, fmt.Errorf(
			"sandbox profile %q: %s", root.Name, parts.filesystemConflicts[0])
	}
	if len(parts.network.Deny) > MaxNetworkAllowEntries {
		return Profile{}, nil, fmt.Errorf(
			"flattened network.deny has too many entries (maximum %d)",
			MaxNetworkAllowEntries)
	}
	out := Profile{
		Name:                    root.Name,
		PreLaunch:               clonePreLaunch(parts.preLaunch),
		Filesystem:              make([]FilesystemGrant, 0, len(parts.filesystem)),
		Environment:             make([]EnvironmentEntry, 0, len(parts.environment)),
		AgentDirectories:        make([]string, 0, len(parts.agentDirectories)),
		FilesystemRoot:          parts.filesystemRoot,
		NetworkAccess:           parts.networkAccess,
		ResourceLimits:          parts.resourceLimits,
		DarwinAllowMachRegister: parts.darwinAllowMachRegister,
	}
	if parts.hasFilesystemSpellings {
		out.FilesystemSpellings = &FilesystemSpellings{
			Version: FilesystemSpellingsVersion,
			Rules:   []FilesystemSpellingRule{},
		}
	}
	if parts.hasNewNetwork {
		network := cloneNetworkRules(parts.network)
		// Flatten returns a self-contained authoring profile that Resolve will
		// normalize again. Reconstruct the compositional baseline whenever the
		// merged result carries denies; Mode+Deny is reserved for effective
		// snapshots and must remain invalid at the authoring boundary.
		if len(network.Deny) > 0 {
			switch network.Mode {
			case AccessModeOpen:
				network.Baseline = NetworkBaselineAllow
			case AccessModeList, AccessModeClosed:
				network.Baseline = NetworkBaselineDeny
			default:
				return Profile{}, nil, fmt.Errorf(
					"flattened network denies require a concrete baseline")
			}
			network.Mode = AccessModeUnset
		}
		out.Network = &network
		out.NetworkAccess = LegacyNetworkAccessForExport(out.Network, out.NetworkAccess)
	}
	if parts.hasNewUnixSockets ||
		(parts.hasNewNetwork && parts.unixSockets.Mode != AccessModeUnset) {
		sockets := cloneUnixSocketRules(parts.unixSockets)
		out.UnixSockets = &sockets
	}
	spelledResolved := map[string]struct{}{}
	for _, grant := range parts.filesystem {
		out.Filesystem = append(out.Filesystem, grant)
		if out.FilesystemSpellings == nil {
			continue
		}
		// Spellings belong to the host path, so two mounts of one directory
		// contribute a single spelling rule rather than a duplicate row.
		if _, done := spelledResolved[grant.Path]; done {
			continue
		}
		set := parts.filesystemSpellings[grant.Path]
		if len(set) == 0 {
			continue
		}
		spelledResolved[grant.Path] = struct{}{}
		spellings := make([]string, 0, len(set))
		for spelling := range set {
			spellings = append(spellings, spelling)
		}
		sort.Strings(spellings)
		out.FilesystemSpellings.Rules = append(
			out.FilesystemSpellings.Rules,
			FilesystemSpellingRule{ResolvedPath: grant.Path, Spellings: spellings},
		)
	}
	for _, entry := range parts.environment {
		out.Environment = append(out.Environment, entry)
	}
	for name := range parts.agentDirectories {
		out.AgentDirectories = append(out.AgentDirectories, name)
	}
	sortFilesystemGrants(out.Filesystem)
	if out.FilesystemSpellings != nil {
		sort.Slice(out.FilesystemSpellings.Rules, func(i, j int) bool {
			return out.FilesystemSpellings.Rules[i].ResolvedPath <
				out.FilesystemSpellings.Rules[j].ResolvedPath
		})
	}
	sort.Slice(out.Environment, func(i, j int) bool { return out.Environment[i].Name < out.Environment[j].Name })
	sort.Strings(out.AgentDirectories)
	return out, accessCompositionNotices(parts), nil
}

type flattenedParts struct {
	filesystem          map[string]FilesystemGrant
	filesystemSpellings map[string]map[string]struct{}
	// filesystemConflicts records overrides that replace one HOST path's rule
	// with a rule about a DIFFERENT host path at the same sandbox path. Ordinary
	// same-host-path override is settled include semantics; this shape is not,
	// because the overridden rule simply vanishes — and if it was a deny, the
	// composed profile ends up neither denying nor granting that host path, with
	// nothing downstream able to notice. Refuse instead.
	filesystemConflicts     []string
	hasFilesystemSpellings  bool
	environment             map[string]EnvironmentEntry
	agentDirectories        map[string]struct{}
	filesystemRoot          FilesystemRootMode
	networkAccess           NetworkAccess
	network                 NetworkRules
	unixSockets             UnixSocketRules
	hasNewNetwork           bool
	hasNewUnixSockets       bool
	resourceLimits          ResourceLimits
	darwinAllowMachRegister bool
	// preLaunch keeps composed blocks in execution order. Unlike every other
	// merged field it is a SLICE, not a map: an override replaces a same-named
	// block in place rather than re-keying it, because these are sequential
	// statements and reordering them would change what they do.
	preLaunch               []PreLaunchBlock
	networkListContributors []string
	socketListContributors  []string
}

// noteFilesystemOverride records the one override shape include composition
// must not perform silently: replacing the rule for one HOST path with a rule
// about a different host path that happens to occupy the same sandbox path.
func (parts *flattenedParts) noteFilesystemOverride(guest string, grant FilesystemGrant) {
	previous, exists := parts.filesystem[guest]
	if !exists || previous.Path == grant.Path {
		return
	}
	parts.filesystemConflicts = append(parts.filesystemConflicts, fmt.Sprintf(
		"sandbox path %q is claimed by two different host paths %q and %q across includes; an include override may not replace a rule about one host path with a rule about another",
		guest, previous.Path, grant.Path))
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
		filesystem:          map[string]FilesystemGrant{},
		filesystemSpellings: map[string]map[string]struct{}{},
		environment:         map[string]EnvironmentEntry{},
		agentDirectories:    map[string]struct{}{},
	}
	for _, name := range p.Includes {
		parts, done := f.memo[name]
		if !done {
			parts = f.compose(f.profiles[name])
			f.memo[name] = parts
		}
		for guest, grant := range parts.filesystem {
			out.noteFilesystemOverride(guest, grant)
			out.filesystem[guest] = grant
		}
		mergeFlattenedFilesystemSpellings(out.filesystemSpellings, parts.filesystemSpellings)
		out.hasFilesystemSpellings =
			out.hasFilesystemSpellings || parts.hasFilesystemSpellings
		for name, entry := range parts.environment {
			delete(out.agentDirectories, name)
			out.environment[name] = entry
		}
		for name := range parts.agentDirectories {
			delete(out.environment, name)
			out.agentDirectories[name] = struct{}{}
		}
		if filesystemRootRank(parts.filesystemRoot) > filesystemRootRank(out.filesystemRoot) {
			out.filesystemRoot = parts.filesystemRoot
		}
		if parts.networkAccess != NetworkAccessInherit {
			out.networkAccess = parts.networkAccess
		}
		if parts.resourceLimits.Memory != "" {
			out.resourceLimits.Memory = parts.resourceLimits.Memory
		}
		if parts.resourceLimits.CPU != nil {
			value := *parts.resourceLimits.CPU
			out.resourceLimits.CPU = &value
		}
		out.network = intersectNetworkRules(out.network, parts.network)
		out.unixSockets = intersectUnixSocketRules(out.unixSockets, parts.unixSockets)
		out.hasNewNetwork = out.hasNewNetwork || parts.hasNewNetwork
		out.hasNewUnixSockets = out.hasNewUnixSockets || parts.hasNewUnixSockets
		out.preLaunch = mergePreLaunch(out.preLaunch, parts.preLaunch)
		out.networkListContributors = appendUniqueStrings(
			out.networkListContributors, parts.networkListContributors...)
		out.socketListContributors = appendUniqueStrings(
			out.socketListContributors, parts.socketListContributors...)
	}
	out.preLaunch = mergePreLaunch(out.preLaunch, p.PreLaunch)
	for _, grant := range p.Filesystem {
		// Include override is keyed on the guest path: "the same rule" means the
		// same position inside the sandbox. For an unremapped rule that is the
		// host path, so include semantics are unchanged; a remapped rule
		// overrides only the mount that occupies its own sandbox path.
		out.noteFilesystemOverride(grant.GuestPath(), grant)
		out.filesystem[grant.GuestPath()] = grant
	}
	if p.FilesystemSpellings != nil {
		out.hasFilesystemSpellings = true
		for _, rule := range p.FilesystemSpellings.Rules {
			set := out.filesystemSpellings[rule.ResolvedPath]
			if set == nil {
				set = map[string]struct{}{}
				out.filesystemSpellings[rule.ResolvedPath] = set
			}
			for _, spelling := range rule.Spellings {
				set[spelling] = struct{}{}
			}
		}
	}
	for _, entry := range p.Environment {
		delete(out.agentDirectories, entry.Name)
		out.environment[entry.Name] = entry
	}
	for _, name := range p.AgentDirectories {
		delete(out.environment, name)
		out.agentDirectories[name] = struct{}{}
	}
	if filesystemRootRank(p.FilesystemRoot) > filesystemRootRank(out.filesystemRoot) {
		out.filesystemRoot = p.FilesystemRoot
	}
	if p.NetworkAccess != NetworkAccessInherit {
		out.networkAccess = p.NetworkAccess
	}
	if p.ResourceLimits.Memory != "" {
		out.resourceLimits.Memory = p.ResourceLimits.Memory
	}
	if p.ResourceLimits.CPU != nil {
		value := *p.ResourceLimits.CPU
		out.resourceLimits.CPU = &value
	}
	out.darwinAllowMachRegister = out.darwinAllowMachRegister || p.DarwinAllowMachRegister
	own := composeProfileAccessAxes(p)
	out.network = intersectNetworkRules(out.network, own.network)
	out.unixSockets = intersectUnixSocketRules(out.unixSockets, own.unixSockets)
	out.hasNewNetwork = out.hasNewNetwork || own.hasNewNetwork
	out.hasNewUnixSockets = out.hasNewUnixSockets || own.hasNewUnixSockets
	out.networkListContributors = appendUniqueStrings(
		out.networkListContributors, own.networkListContributors...)
	out.socketListContributors = appendUniqueStrings(
		out.socketListContributors, own.socketListContributors...)
	return out
}

func mergeFlattenedFilesystemSpellings(
	dst, src map[string]map[string]struct{},
) {
	for resolved, spellings := range src {
		set := dst[resolved]
		if set == nil {
			set = map[string]struct{}{}
			dst[resolved] = set
		}
		for spelling := range spellings {
			set[spelling] = struct{}{}
		}
	}
}

func composeProfileAccessAxes(p Profile) *flattenedParts {
	axes, err := DeriveAccessAxes(p)
	if err != nil {
		// compose only receives normalized profiles; keep this helper pure and
		// fail closed if a future caller violates that invariant.
		panic("compose unnormalized sandbox profile access axes: " + err.Error())
	}
	out := &flattenedParts{
		network:           axes.Network,
		unixSockets:       axes.UnixSockets,
		hasNewNetwork:     p.Network != nil,
		hasNewUnixSockets: p.UnixSockets != nil,
	}
	if axes.Network.Mode == AccessModeList {
		out.networkListContributors = []string{p.Name}
	}
	if axes.UnixSockets.Mode == AccessModeList {
		out.socketListContributors = []string{p.Name}
	}
	return out
}

func accessCompositionNotices(parts *flattenedParts) []AccessNotice {
	out := []AccessNotice{}
	if parts.network.Mode == AccessModeList && len(parts.network.Allow) == 0 {
		out = append(out, compositionNotice("network", parts.networkListContributors))
	}
	if parts.unixSockets.Mode == AccessModeList && len(parts.unixSockets.Allow) == 0 {
		out = append(out, compositionNotice("unix_sockets", parts.socketListContributors))
	}
	return out
}

func appendUniqueStrings(dst []string, values ...string) []string {
	seen := make(map[string]struct{}, len(dst)+len(values))
	for _, value := range dst {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		dst = append(dst, value)
	}
	return dst
}
