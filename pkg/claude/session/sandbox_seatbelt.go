package session

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"golang.org/x/text/unicode/norm"
)

// seatbeltFileIdentity is the Darwin lstat identity used to decide whether
// case/NFC-equivalent spellings really name one filesystem object. APFS may be
// case-sensitive, so folding alone may only nominate a merge.
type seatbeltFileIdentity struct {
	dev uint64
	ino uint64
}

type seatbeltIdentityLookup func(string) (seatbeltFileIdentity, bool)

type seatbeltProfileParam struct {
	name string
	path string
}

type seatbeltRegion struct {
	path             string
	mode             sandboxpolicy.MountMode
	policy           bool
	networkException bool
	denyBoundary     bool
	daemonReopen     bool
	unshadowable     bool
}

type seatbeltRegionNode struct {
	seatbeltRegion
	parent   int
	children []int
}

// renderSeatbeltProfile compiles the ordered mount contract into deny-only
// Seatbelt regions. Seatbelt rule precedence depends on predicate specificity,
// so replaying an ancestor deny followed by a descendant allow would leave the
// MountPlan contract implicit in platform rule selection. Instead this compiler
// first resolves the four precedence classes, then carves every final positive
// descendant out of the deny predicate that covers its ancestor.
//
// runtimeTempDir is the canonical Darwin $TMPDIR. Its write carveout, plus the
// fixed /dev runtime carveouts, pierces only the class-1 root write deny. A
// narrower profile RO/hide still produces its own deny without those
// exceptions, so runtime compatibility can never reopen an operator policy.
//
// proxyEndpoint is the host-loopback address the tclaude filtering proxy
// listens on, and is required by — and permitted on — exactly the plans that
// deploy one. It is the zero value for every other plan.
func renderSeatbeltProfile(
	phase0WriteDirs, requiredSocketPaths []string,
	plan sandboxpolicy.MountPlan,
	proxyEndpoint netip.AddrPort,
	protectedRoots []string,
	tmuxSocketDir, runtimeTempDir string,
	identity seatbeltIdentityLookup,
	daemonReadOnlyPaths []string,
	privateWriteDirs ...TclaudeLayerPrivateWriteDir,
) (string, []seatbeltProfileParam, error) {
	deploysProxy := tclaudeLayerPlanDeploysProxy(plan)
	switch plan.NetworkPosture {
	case sandboxpolicy.NetworkHostOpen, sandboxpolicy.NetworkIsolatedWithAgentd:
	case sandboxpolicy.NetworkFiltered:
		// Both filtered renderings need a compiled policy, so the guard stays
		// ahead of the branch rather than inside one arm of it. The proxy floor
		// opens nothing without it, but a filtered plan carrying no policy at
		// all is a caller that failed to materialize, and this renderer should
		// fail closed on its own rather than rely on the launch seam's check.
		if plan.FilteredNetwork == nil {
			return "", nil, fmt.Errorf(
				"darwin tclaude-layer filtered networking requires a compiled network policy",
			)
		}
		// The two filtered renderings are mutually exclusive by construction
		// rather than by ordering here: a loopback-only compiled rule set
		// carries no deny rows, so it is not discriminating and never resolves
		// to an engine at all (NetworkRulesAreDiscriminating). Only a rule set
		// the native Seatbelt rules cannot express reaches the proxy floor.
		if !deploysProxy &&
			!sandboxpolicy.FilteredNetworkRulesAreLoopbackOnly(plan.FilteredNetwork) {
			return "", nil, fmt.Errorf(
				"darwin tclaude-layer filtered networking supports only a non-empty loopback-only list",
			)
		}
	default:
		return "", nil, fmt.Errorf(
			"darwin tclaude-layer has invalid network posture %d",
			plan.NetworkPosture,
		)
	}
	if err := validateSeatbeltProxyEndpoint(proxyEndpoint, deploysProxy); err != nil {
		return "", nil, err
	}
	if identity == nil {
		identity = func(string) (seatbeltFileIdentity, bool) {
			return seatbeltFileIdentity{}, false
		}
	}

	contract, err := cleanSeatbeltPaths("launch-contract write", phase0WriteDirs)
	if err != nil {
		return "", nil, err
	}
	protected, err := cleanSeatbeltPaths("protected root", protectedRoots)
	if err != nil {
		return "", nil, err
	}
	tmuxSocketDir, err = cleanSeatbeltPath("tmux socket directory", tmuxSocketDir)
	if err != nil {
		return "", nil, err
	}
	runtimeTempDir, err = cleanSeatbeltPath("darwin runtime TMPDIR", runtimeTempDir)
	if err != nil {
		return "", nil, err
	}
	daemonReadOnly, err := cleanSeatbeltPaths(
		"daemon-final read-only path", daemonReadOnlyPaths)
	if err != nil {
		return "", nil, err
	}

	// Class 1 starts from the host root read-only, then reopens the launch
	// contract. Class 3 is established before class-2 replay and nothing
	// reopens beneath it: TCL-791 removed break-glass, the one former
	// exception, so a protected hide here is final.
	ordered := []seatbeltRegion{{path: string(filepath.Separator), mode: sandboxpolicy.MountRO}}
	for _, path := range contract {
		ordered = append(ordered, seatbeltRegion{path: path, mode: sandboxpolicy.MountRW})
	}
	requiredSockets, err := cleanSeatbeltPaths(
		"required unix socket",
		requiredSocketPaths,
	)
	if err != nil {
		return "", nil, err
	}
	for _, path := range requiredSockets {
		ordered = append(ordered, seatbeltRegion{
			path:             path,
			mode:             sandboxpolicy.MountRO,
			networkException: true,
		})
	}
	for _, path := range protected {
		ordered = append(ordered, seatbeltRegion{path: path, mode: sandboxpolicy.MountHide})
	}

	for i, entry := range plan.Entries {
		// Seatbelt is a path filter over the real host namespace, not a mount
		// namespace: it can allow or deny a path, but it cannot make a directory
		// appear somewhere else. Refuse rather than approximate — binding at the
		// host path instead would expose a path the operator did not authorize
		// while leaving the one they did authorize empty. This is the same
		// refusal darwinSeatbeltReadOnlyPaths already makes for daemon-final
		// source→target binds.
		if entry.IsRemapped() {
			return "", nil, fmt.Errorf(
				"seatbelt_mount_path_projection: Seatbelt cannot project host path %q onto sandbox path %q (mount plan entry %d); mount paths require a mount namespace, which only the Linux tclaude-layer provides",
				entry.SourcePath(), entry.Path, i)
		}
		path, cleanErr := cleanSeatbeltPath(fmt.Sprintf("mount plan entry %d", i), entry.Path)
		if cleanErr != nil {
			return "", nil, cleanErr
		}
		switch entry.Mode {
		case sandboxpolicy.MountRO, sandboxpolicy.MountRW, sandboxpolicy.MountHide:
		default:
			return "", nil, fmt.Errorf("mount plan entry %d has invalid mode %d", i, entry.Mode)
		}
		ordered = append(ordered, seatbeltRegion{
			path:   path,
			mode:   entry.Mode,
			policy: true,
		})

		switch entry.Mode {
		case sandboxpolicy.MountRO, sandboxpolicy.MountRW:
			ordered = appendSeatbeltProtectedRehides(ordered, path, protected, identity)
		case sandboxpolicy.MountHide:
			for _, writeDir := range contract {
				if !seatbeltSamePath(path, writeDir, identity) &&
					seatbeltPathContains(path, writeDir, identity) {
					ordered = append(ordered, seatbeltRegion{
						path: writeDir,
						mode: sandboxpolicy.MountRW,
					})
					ordered = appendSeatbeltProtectedRehides(
						ordered,
						writeDir,
						protected,
						identity,
					)
				}
			}
			for _, requiredSocket := range requiredSockets {
				if !seatbeltSamePath(path, requiredSocket, identity) &&
					seatbeltPathContains(path, requiredSocket, identity) {
					ordered = append(ordered, seatbeltRegion{
						path:             requiredSocket,
						mode:             sandboxpolicy.MountRO,
						networkException: true,
					})
					ordered = appendSeatbeltProtectedRehides(
						ordered,
						requiredSocket,
						protected,
						identity,
					)
				}
			}
		}
	}

	// This shared daemon-owned parent is a hide region like every other hide:
	// its filesystem read deny and remote-unix network deny receive the same
	// descendant exceptions. Only the current session's child is carved out.
	for i, privateDir := range privateWriteDirs {
		parent, cleanErr := cleanSeatbeltPath(
			fmt.Sprintf("private write entry %d parent", i),
			privateDir.Parent,
		)
		if cleanErr != nil {
			return "", nil, cleanErr
		}
		current, cleanErr := cleanSeatbeltPath(
			fmt.Sprintf("private write entry %d current", i),
			privateDir.Current,
		)
		if cleanErr != nil {
			return "", nil, cleanErr
		}
		ordered = append(ordered,
			seatbeltRegion{
				path: parent, mode: sandboxpolicy.MountHide,
				// The parent is nested below the protected daemon-data hide,
				// but it remains its own deny boundary. This later,
				// daemon-authored exception must beat any policy carveout
				// while still reopening exactly the current child.
				denyBoundary: true,
			},
			seatbeltRegion{
				path: current, mode: sandboxpolicy.MountRW,
				daemonReopen: true,
			},
		)
	}

	// A daemon-final read-only bind is a recursive write boundary. Seatbelt
	// cannot project one host path onto another, so the Darwin adapter admits
	// only same-path binds and supplies their targets here. unshadowable keeps
	// an earlier, more-specific profile RW row from piercing the boundary.
	// A profile hide below the path remains a separate read/write/connect deny.
	for _, path := range daemonReadOnly {
		ordered = append(ordered, seatbeltRegion{
			path: path, mode: sandboxpolicy.MountRO, unshadowable: true,
		})
	}

	// Class 4 is last and receives no carveout at all.
	ordered = append(ordered, seatbeltRegion{
		path:         tmuxSocketDir,
		mode:         sandboxpolicy.MountHide,
		unshadowable: true,
	})

	ordered, err = expandSeatbeltAliasRegions(ordered, plan.Aliases)
	if err != nil {
		return "", nil, err
	}
	nodes := buildSeatbeltRegionTree(ordered, identity)
	profile, params := compileSeatbeltDenyRegions(
		nodes,
		runtimeTempDir,
		plan,
		proxyEndpoint,
		identity,
	)
	return profile, params, nil
}

// validateSeatbeltProxyEndpoint holds the proxy floor's one IP destination to
// the plan that actually deploys a proxy, in both directions.
//
// A proxy plan without an endpoint is refused rather than rendered as the bare
// isolated floor: the harness would come up with no route to its own filtering
// proxy, which is the deny-everything outcome dressed as a working launch. An
// endpoint supplied for a plan that deploys no proxy is refused too, because
// silently dropping it would open nothing while the caller believed it had.
//
// Membership in the host-loopback space is decided by the shared predicate
// (sandboxpolicy.AddrIsLoopbackIdentity), never by a second local notion of
// what "loopback" spells. An endpoint outside that space would not be a
// filtering proxy on this host at all; it would be a route off it.
//
// The unspecified address is the one place where reusing that predicate alone
// would be wrong, and the reason is that it answers a question this endpoint
// does not ask. AddrIsLoopbackIdentity carries 0.0.0.0/8 and ::/128 because
// connecting TO the unspecified address lands on local loopback, which makes
// them host-loopback DESTINATIONS. This endpoint is where the proxy LISTENS,
// and binding to the unspecified address is the opposite: a wildcard listener
// on every interface, so the sandbox's only egress would also be reachable
// from the LAN. The predicate stays the sole definition of loopback identity;
// this is a second question about the same address, answered here.
func validateSeatbeltProxyEndpoint(
	endpoint netip.AddrPort,
	deploysProxy bool,
) error {
	if !deploysProxy {
		if endpoint.Addr().IsValid() || endpoint.Port() != 0 {
			return fmt.Errorf(
				"darwin tclaude-layer was given proxy endpoint %s for a plan that deploys no filtering proxy",
				endpoint,
			)
		}
		return nil
	}
	if !endpoint.Addr().IsValid() {
		return fmt.Errorf(
			"darwin tclaude-layer proxy floor requires the host-loopback endpoint the filtering proxy listens on",
		)
	}
	if endpoint.Port() == 0 {
		return fmt.Errorf(
			"darwin tclaude-layer proxy floor requires a bound proxy port, got %s",
			endpoint,
		)
	}
	if !sandboxpolicy.AddrIsLoopbackIdentity(endpoint.Addr()) {
		return fmt.Errorf(
			"darwin tclaude-layer proxy floor refuses non-host-loopback proxy endpoint %s",
			endpoint,
		)
	}
	if endpoint.Addr().IsUnspecified() {
		return fmt.Errorf(
			"darwin tclaude-layer proxy floor refuses wildcard proxy endpoint %s: the filtering proxy must listen on host loopback, not on every interface",
			endpoint,
		)
	}
	return nil
}

func cleanSeatbeltPaths(label string, paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for i, path := range paths {
		clean, err := cleanSeatbeltPath(fmt.Sprintf("%s %d", label, i), path)
		if err != nil {
			return nil, err
		}
		out = append(out, clean)
	}
	return out, nil
}

func cleanSeatbeltPath(label, path string) (string, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s has non-absolute path %q", label, path)
	}
	return path, nil
}

func appendSeatbeltProtectedRehides(
	regions []seatbeltRegion,
	mountedPath string,
	protectedRoots []string,
	identity seatbeltIdentityLookup,
) []seatbeltRegion {
	for _, protected := range protectedRoots {
		if seatbeltPathContains(mountedPath, protected, identity) ||
			seatbeltPathContains(protected, mountedPath, identity) {
			regions = append(regions, seatbeltRegion{
				path: protected,
				mode: sandboxpolicy.MountHide,
			})
		}
	}
	return regions
}

func seatbeltFoldedPath(path string) string {
	return norm.NFC.String(strings.ToLower(filepath.Clean(path)))
}

func seatbeltSamePath(a, b string, identity seatbeltIdentityLookup) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if a == b {
		return true
	}
	if seatbeltFoldedPath(a) != seatbeltFoldedPath(b) {
		return false
	}
	if identity == nil {
		return false
	}
	aID, aOK := identity(a)
	bID, bOK := identity(b)
	return aOK && bOK && aID == bID
}

// seatbeltPathContains accepts byte-exact containment immediately. A
// case/NFC-folded relation is only a nomination: the candidate ancestor and
// the corresponding prefix of target must have the same lstat dev+ino before
// the relation affects policy. Distinct or unknowable identities stay as
// separate regions for case-sensitive APFS.
func seatbeltPathContains(
	dir, target string,
	identity seatbeltIdentityLookup,
) bool {
	dir = filepath.Clean(dir)
	target = filepath.Clean(target)
	if sandboxpolicy.PathContainsOrEqual(dir, target) {
		return true
	}
	foldedDir := seatbeltFoldedPath(dir)
	foldedTarget := seatbeltFoldedPath(target)
	if !sandboxpolicy.PathContainsOrEqual(foldedDir, foldedTarget) {
		return false
	}
	if identity == nil {
		return false
	}

	dirParts := seatbeltPathParts(dir)
	targetParts := seatbeltPathParts(target)
	if len(dirParts) > len(targetParts) {
		return false
	}
	prefix := string(filepath.Separator)
	if len(dirParts) > 0 {
		prefix = filepath.Join(append([]string{string(filepath.Separator)}, targetParts[:len(dirParts)]...)...)
	}
	dirID, dirOK := identity(dir)
	prefixID, prefixOK := identity(prefix)
	return dirOK && prefixOK && dirID == prefixID
}

func seatbeltPathParts(path string) []string {
	path = filepath.Clean(path)
	if path == string(filepath.Separator) {
		return nil
	}
	return strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
}

func expandSeatbeltAliasRegions(
	regions []seatbeltRegion,
	aliases []sandboxpolicy.MountAlias,
) ([]seatbeltRegion, error) {
	if len(aliases) == 0 {
		return regions, nil
	}
	cleanAliases := make([]sandboxpolicy.MountAlias, 0, len(aliases))
	for i, alias := range aliases {
		link, err := cleanSeatbeltPath(fmt.Sprintf("mount alias %d link", i), alias.Link)
		if err != nil {
			return nil, err
		}
		target, err := cleanSeatbeltPath(fmt.Sprintf("mount alias %d target", i), alias.Target)
		if err != nil {
			return nil, err
		}
		cleanAliases = append(cleanAliases, sandboxpolicy.MountAlias{
			Link:   link,
			Target: target,
		})
	}
	out := make([]seatbeltRegion, 0, len(regions)*(len(aliases)+1))
	for _, region := range regions {
		spellings := map[string]bool{region.path: true}
		type aliasCandidate struct {
			path string
			used map[int]bool
		}
		queue := []aliasCandidate{{path: region.path, used: map[int]bool{}}}
		for len(queue) > 0 {
			candidate := queue[0]
			queue = queue[1:]
			for aliasIndex, alias := range cleanAliases {
				if candidate.used[aliasIndex] {
					continue
				}
				target := filepath.Clean(alias.Target)
				if !sandboxpolicy.PathContainsOrEqual(target, candidate.path) {
					continue
				}
				rel, err := filepath.Rel(target, candidate.path)
				if err != nil {
					continue
				}
				spelling := filepath.Clean(filepath.Join(alias.Link, rel))
				if !spellings[spelling] {
					spellings[spelling] = true
					used := make(map[int]bool, len(candidate.used)+1)
					for usedIndex := range candidate.used {
						used[usedIndex] = true
					}
					used[aliasIndex] = true
					queue = append(queue, aliasCandidate{path: spelling, used: used})
				}
			}
		}
		paths := make([]string, 0, len(spellings))
		for path := range spellings {
			paths = append(paths, path)
		}
		sort.Slice(paths, func(i, j int) bool {
			left, right := seatbeltFoldedPath(paths[i]), seatbeltFoldedPath(paths[j])
			if left != right {
				return left < right
			}
			return paths[i] < paths[j]
		})
		for _, path := range paths {
			out = append(out, seatbeltRegion{
				path:             path,
				mode:             region.mode,
				policy:           region.policy,
				networkException: region.networkException,
				denyBoundary:     region.denyBoundary,
				daemonReopen:     region.daemonReopen,
				unshadowable:     region.unshadowable,
			})
		}
	}
	return out, nil
}

func buildSeatbeltRegionTree(
	ordered []seatbeltRegion,
	identity seatbeltIdentityLookup,
) []seatbeltRegionNode {
	// Exact paths always merge. Folded spellings merge only after identity
	// confirmation; the later replayed region supplies both spelling and mode.
	regions := make([]seatbeltRegion, 0, len(ordered))
	for _, candidate := range ordered {
		replaced := false
		for i := range regions {
			if seatbeltSamePath(regions[i].path, candidate.path, identity) {
				if candidate.mode != sandboxpolicy.MountHide {
					candidate.networkException = candidate.networkException ||
						regions[i].networkException
				}
				regions[i] = candidate
				replaced = true
				break
			}
		}
		if !replaced {
			regions = append(regions, candidate)
		}
	}
	sort.Slice(regions, func(i, j int) bool {
		left, right := seatbeltFoldedPath(regions[i].path), seatbeltFoldedPath(regions[j].path)
		if left != right {
			return left < right
		}
		return regions[i].path < regions[j].path
	})

	nodes := make([]seatbeltRegionNode, len(regions))
	for i, region := range regions {
		nodes[i] = seatbeltRegionNode{seatbeltRegion: region, parent: -1}
	}
	for i := range nodes {
		best := -1
		bestDepth := -1
		for candidate := range nodes {
			if candidate == i ||
				seatbeltSamePath(nodes[candidate].path, nodes[i].path, identity) ||
				!seatbeltPathContains(nodes[candidate].path, nodes[i].path, identity) {
				continue
			}
			depth := len(seatbeltPathParts(nodes[candidate].path))
			if depth > bestDepth ||
				(depth == bestDepth && (best == -1 || nodes[candidate].path < nodes[best].path)) {
				best = candidate
				bestDepth = depth
			}
		}
		nodes[i].parent = best
		if best >= 0 {
			nodes[best].children = append(nodes[best].children, i)
		}
	}
	for i := range nodes {
		sort.Slice(nodes[i].children, func(left, right int) bool {
			return nodes[nodes[i].children[left]].path < nodes[nodes[i].children[right]].path
		})
	}
	return nodes
}

func compileSeatbeltDenyRegions(
	nodes []seatbeltRegionNode,
	runtimeTempDir string,
	plan sandboxpolicy.MountPlan,
	proxyEndpoint netip.AddrPort,
	identity seatbeltIdentityLookup,
) (string, []seatbeltProfileParam) {
	var profile strings.Builder
	profile.WriteString("(version 1)\n")
	profile.WriteString("(allow default)\n\n")
	profile.WriteString("; Filesystem policy is deny-only. Positive descendants are carved out\n")
	profile.WriteString("; inside each deny predicate so plan precedence does not depend on\n")
	profile.WriteString("; Seatbelt allow/deny rule selection.\n")

	params := []seatbeltProfileParam{}
	// The floor a plan builds, not the posture it names. TclaudeLayerFloorPosture
	// is the same mapping the Linux launcher applies, and it says once — there
	// rather than in a second switch here — that the proxy engine's floor IS the
	// isolated posture's construction. On Seatbelt that means the isolated
	// denies verbatim, with the proxy endpoint as their only addition; a
	// filtered plan reaches the native loopback rules only when it deploys no
	// proxy at all.
	switch tclaudeLayerPlanFloorPosture(plan) {
	case sandboxpolicy.NetworkIsolatedWithAgentd:
		params = appendSeatbeltIsolatedNetworkRules(
			&profile, params, nodes, proxyEndpoint)
	case sandboxpolicy.NetworkFiltered:
		appendSeatbeltLoopbackNetworkRules(&profile, plan.FilteredNetwork)
	}
	writeRules := seatbeltDenyStarts(nodes, func(mode sandboxpolicy.MountMode) bool {
		return mode != sandboxpolicy.MountRW
	})
	normalWriteRule := make(map[int]bool, len(writeRules))
	for index, nodeIndex := range writeRules {
		normalWriteRule[nodeIndex] = true
		exceptions := seatbeltDenyExceptions(
			nodes,
			nodeIndex,
			func(mode sandboxpolicy.MountMode) bool { return mode != sandboxpolicy.MountRW },
		)
		rootBaseline := nodes[nodeIndex].parent == -1 &&
			nodes[nodeIndex].path == string(filepath.Separator)
		params = appendSeatbeltDenyRule(
			&profile,
			params,
			"file-write*",
			fmt.Sprintf("WRITE_DENY_%d", index),
			nodes[nodeIndex].path,
			exceptions,
			nodes,
			rootBaseline,
			runtimeTempDir,
		)
	}
	// A policy deny that overlaps a baseline runtime carveout must receive its
	// own no-carveout deny even when the surrounding root is already read-only.
	// This is the load-bearing distinction between process compatibility and
	// operator authority: /dev and TMPDIR are writable only until a profile
	// explicitly makes their region RO/hidden.
	for _, nodeIndex := range seatbeltRuntimePolicyDenies(
		nodes,
		runtimeTempDir,
		normalWriteRule,
		identity,
	) {
		index := len(writeRules)
		writeRules = append(writeRules, nodeIndex)
		exceptions := seatbeltFirstAllowedDescendants(
			nodes,
			nodeIndex,
			func(mode sandboxpolicy.MountMode) bool { return mode != sandboxpolicy.MountRW },
		)
		params = appendSeatbeltDenyRule(
			&profile,
			params,
			"file-write*",
			fmt.Sprintf("WRITE_DENY_%d", index),
			nodes[nodeIndex].path,
			exceptions,
			nodes,
			false,
			runtimeTempDir,
		)
	}

	readRules := seatbeltDenyStarts(nodes, func(mode sandboxpolicy.MountMode) bool {
		return mode == sandboxpolicy.MountHide
	})
	for index, nodeIndex := range readRules {
		exceptions := seatbeltDenyExceptions(
			nodes,
			nodeIndex,
			func(mode sandboxpolicy.MountMode) bool { return mode == sandboxpolicy.MountHide },
		)
		params = appendSeatbeltDenyRule(
			&profile,
			params,
			"file-read*",
			fmt.Sprintf("READ_DENY_%d", index),
			nodes[nodeIndex].path,
			exceptions,
			nodes,
			false,
			runtimeTempDir,
		)
		appendSeatbeltUnixConnectDenyRule(
			&profile,
			fmt.Sprintf("READ_DENY_%d", index),
			exceptions,
			nodes,
		)
	}
	return profile.String(), params
}

func seatbeltDenyExceptions(
	nodes []seatbeltRegionNode,
	root int,
	denied func(sandboxpolicy.MountMode) bool,
) []int {
	switch {
	case nodes[root].unshadowable:
		return nil
	case nodes[root].denyBoundary:
		return seatbeltDaemonReopenDescendants(nodes, root)
	default:
		return seatbeltFirstAllowedDescendants(nodes, root, denied)
	}
}

func seatbeltDaemonReopenDescendants(
	nodes []seatbeltRegionNode,
	root int,
) []int {
	out := []int{}
	var walk func(int)
	walk = func(nodeIndex int) {
		for _, child := range nodes[nodeIndex].children {
			if nodes[child].daemonReopen {
				out = append(out, child)
				continue
			}
			walk(child)
		}
	}
	walk(root)
	sort.Slice(out, func(i, j int) bool {
		return nodes[out[i]].path < nodes[out[j]].path
	})
	return out
}

// appendSeatbeltLoopbackNetworkRules applies the one network list Seatbelt can
// represent without a proxy. The remote-ip wildcard confines the deny to IP
// traffic, preserving the independently authored Unix-socket axis. Port-scoped
// exceptions use the narrower TCP/UDP predicates so Seatbelt selects them over
// the IP-wide deny; a portless loopback rule can use the IP predicate directly.
//
// Outbound exceptions must be remote predicates. A local-ip predicate observes
// the unbound socket's source address and Seatbelt treats localhost as matching
// INADDR_ANY, which would admit every destination.
func appendSeatbeltLoopbackNetworkRules(
	profile *strings.Builder,
	rules *sandboxpolicy.FilteredNetworkRuleSet,
) {
	allowAllPorts := false
	portSet := map[int]struct{}{}
	for _, rule := range rules.Rules {
		if len(rule.Ports) == 0 {
			allowAllPorts = true
			break
		}
		for _, port := range rule.Ports {
			portSet[port] = struct{}{}
		}
	}
	ports := make([]int, 0, len(portSet))
	for port := range portSet {
		ports = append(ports, port)
	}
	sort.Ints(ports)

	profile.WriteString("\n; Local access permits only real host-loopback IP destinations.\n")
	profile.WriteString("; Bind/inbound and Unix sockets retain their independently authored behavior.\n")
	profile.WriteString("(deny network-outbound (remote ip \"*:*\"))\n")
	if allowAllPorts {
		profile.WriteString("(allow network-outbound (remote ip \"localhost:*\"))\n")
	} else {
		for _, port := range ports {
			fmt.Fprintf(profile,
				"(allow network-outbound (remote tcp \"localhost:%d\"))\n", port)
			fmt.Fprintf(profile,
				"(allow network-outbound (remote udp \"localhost:%d\"))\n", port)
		}
	}
}

// appendSeatbeltIsolatedNetworkRules blocks every connection except connect(2)
// to the parameterized agentd-floor and profile-allowed Unix socket spellings,
// and blocks creation of every listener. The exception belongs inside the
// outbound deny predicate so connectivity does not depend on Seatbelt
// allow/deny rule selection.
//
// Do not replace these operations with network* or deny system-socket.
// Creating an AF_UNIX socket descriptor is a pathless system-socket operation,
// but socket creation is not connectivity; Linux isolated permits it too. A
// network* deny would block the descriptor agentd needs and could not be
// carved back open.
//
// network-inbound is deliberately absent: current Darwin hardware does not
// make it a reliable AF_UNIX reply block, and reply suppression is not part of
// the isolated contract. Listener prevention rests on network-bind. A listening
// descriptor handed in over SCM_RIGHTS would require cooperation from the
// trusted agentd daemon and is outside this boundary's threat model.
//
// A valid proxyEndpoint turns this into the proxy floor, and adds exactly one
// thing to it: TCP to the host-loopback port the tclaude filtering proxy
// listens on, so both carriages the launcher advertises — HTTP CONNECT and
// SOCKS5, one listener — have a route to it and nothing else does. Everything
// the isolated floor denies stays denied, which is what leaves the harness with
// no way around the proxy: a second host-loopback service is a different port
// and stays unreachable, an external address is not loopback at all, a UDP
// datagram matches no exception (the endpoint's is TCP-only), and network-bind
// still refuses every listener.
//
// The exception is written as a port on Seatbelt's "localhost" rather than on
// the address the proxy actually bound, so the port is the only thing that
// discriminates between destinations here. Whether that token means exactly
// the host-loopback interface — covering 127.0.0.1 and ::1 alike, and nothing
// bound at a routable local address — is a runtime Seatbelt behavior that no
// golden can observe, and it is UNVERIFIED until M3.2's smoke measures it on a
// macOS runner. The intent is the same identity folding the evaluator applies
// to loopback destinations; if the smoke contradicts it, this generator is
// what changes. sandboxpolicy.AddrIsLoopbackIdentity governs which endpoints
// are ACCEPTED above and says nothing about what Seatbelt MATCHES here.
func appendSeatbeltIsolatedNetworkRules(
	profile *strings.Builder,
	params []seatbeltProfileParam,
	nodes []seatbeltRegionNode,
	proxyEndpoint netip.AddrPort,
) []seatbeltProfileParam {
	exceptions := make([]int, 0, 1)
	for index, node := range nodes {
		if node.networkException && node.mode != sandboxpolicy.MountHide {
			exceptions = append(exceptions, index)
		}
	}
	sort.Slice(exceptions, func(i, j int) bool {
		left := seatbeltFoldedPath(nodes[exceptions[i]].path)
		right := seatbeltFoldedPath(nodes[exceptions[j]].path)
		if left != right {
			return left < right
		}
		return nodes[exceptions[i]].path < nodes[exceptions[j]].path
	})

	proxyFloor := proxyEndpoint.Port() != 0
	if proxyFloor {
		profile.WriteString("\n; Proxy-floor networking denies host/public connectivity and listeners.\n")
		profile.WriteString("; Allowlisted connects at the parameterized socket spellings are excepted,\n")
		profile.WriteString("; and so is TCP to the one host-loopback port the tclaude filtering proxy\n")
		profile.WriteString("; listens on. A second loopback service, an external address, a UDP\n")
		profile.WriteString("; datagram and every listener stay denied.\n")
	} else {
		profile.WriteString("\n; Isolated networking denies host/public connectivity and listeners.\n")
		profile.WriteString("; Only allowlisted connects at the parameterized socket spellings are excepted.\n")
	}
	profile.WriteString("(deny network-bind)\n")
	if len(exceptions) == 0 && !proxyFloor {
		profile.WriteString("(deny network-outbound)\n")
		return params
	}
	for index, exception := range exceptions {
		name := fmt.Sprintf("AGENTD_SOCKET_%d", index)
		params = append(params, seatbeltProfileParam{
			name: name,
			path: nodes[exception].path,
		})
	}
	appendSeatbeltFloorOutboundDenyRule(profile, len(exceptions), proxyEndpoint)
	return params
}

// appendSeatbeltFloorOutboundDenyRule writes the floor's single outbound deny.
// Every destination the floor permits is a require-not inside it rather than a
// separate allow rule, so reachability never depends on Seatbelt's
// allow/deny rule selection — the same reason the filesystem carveouts live
// inside their deny predicates.
func appendSeatbeltFloorOutboundDenyRule(
	profile *strings.Builder,
	exceptionCount int,
	proxyEndpoint netip.AddrPort,
) {
	profile.WriteString("(deny network-outbound\n")
	profile.WriteString("  (require-all\n")
	for index := range exceptionCount {
		name := fmt.Sprintf("AGENTD_SOCKET_%d", index)
		profile.WriteString("    (require-not\n")
		profile.WriteString("      (remote unix-socket\n")
		profile.WriteString("        (literal (param \"")
		profile.WriteString(name)
		profile.WriteString("\"))))\n")
	}
	if port := proxyEndpoint.Port(); port != 0 {
		// The port is an integer, so nothing operator-controlled is interpolated
		// into the profile text here.
		fmt.Fprintf(profile,
			"    (require-not (remote tcp \"localhost:%d\"))\n", port)
	}
	profile.WriteString("  ))\n")
}

func seatbeltRuntimePolicyDenies(
	nodes []seatbeltRegionNode,
	runtimeTempDir string,
	normal map[int]bool,
	identity seatbeltIdentityLookup,
) []int {
	out := []int{}
	for i, node := range nodes {
		if !node.policy || node.mode == sandboxpolicy.MountRW {
			continue
		}
		// A normal non-root transition already emits a deny without runtime
		// carveouts. Root is special: its normal rule is the compatibility
		// baseline, so a policy at / still needs a second strict deny.
		if normal[i] && node.path != string(filepath.Separator) {
			continue
		}
		if seatbeltRuntimeCarveoutIntersects(node.path, runtimeTempDir, identity) {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return nodes[out[i]].path < nodes[out[j]].path
	})
	return out
}

func seatbeltRuntimeCarveoutIntersects(
	path, runtimeTempDir string,
	identity seatbeltIdentityLookup,
) bool {
	for _, carveout := range []string{"/dev", runtimeTempDir} {
		if seatbeltPathContains(path, carveout, identity) ||
			seatbeltPathContains(carveout, path, identity) {
			return true
		}
	}
	return false
}

func seatbeltDenyStarts(
	nodes []seatbeltRegionNode,
	denied func(sandboxpolicy.MountMode) bool,
) []int {
	out := []int{}
	for i, node := range nodes {
		if !denied(node.mode) {
			continue
		}
		if node.parent >= 0 &&
			denied(nodes[node.parent].mode) &&
			!node.denyBoundary &&
			!node.unshadowable {
			continue
		}
		out = append(out, i)
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := seatbeltFoldedPath(nodes[out[i]].path), seatbeltFoldedPath(nodes[out[j]].path)
		if left != right {
			return left < right
		}
		return nodes[out[i]].path < nodes[out[j]].path
	})
	return out
}

func seatbeltFirstAllowedDescendants(
	nodes []seatbeltRegionNode,
	root int,
	denied func(sandboxpolicy.MountMode) bool,
) []int {
	out := []int{}
	var walk func(int)
	walk = func(nodeIndex int) {
		for _, child := range nodes[nodeIndex].children {
			if !denied(nodes[child].mode) {
				out = append(out, child)
				continue
			}
			walk(child)
		}
	}
	walk(root)
	sort.Slice(out, func(i, j int) bool {
		return nodes[out[i]].path < nodes[out[j]].path
	})
	return out
}

func appendSeatbeltDenyRule(
	profile *strings.Builder,
	params []seatbeltProfileParam,
	action, name, path string,
	exceptions []int,
	nodes []seatbeltRegionNode,
	rootWriteBaseline bool,
	runtimeTempDir string,
) []seatbeltProfileParam {
	params = append(params, seatbeltProfileParam{name: name, path: path})
	profile.WriteString("\n(deny ")
	profile.WriteString(action)
	profile.WriteString("\n  (require-all\n")
	profile.WriteString("    (require-any (literal (param \"")
	profile.WriteString(name)
	profile.WriteString("\")) (subpath (param \"")
	profile.WriteString(name)
	profile.WriteString("\")))\n")

	if rootWriteBaseline {
		// These are process-runtime compatibility paths, not policy authority.
		// They pierce only this root baseline rule. A narrower RO/hide rule has
		// no such carveouts and therefore still wins.
		profile.WriteString("    (require-not (literal \"/dev/null\"))\n")
		profile.WriteString("    (require-not (literal \"/dev/tty\"))\n")
		profile.WriteString("    (require-not (literal \"/dev/ptmx\"))\n")
		profile.WriteString("    (require-not (literal \"/dev/fd\"))\n")
		profile.WriteString("    (require-not (subpath \"/dev/fd\"))\n")
		profile.WriteString("    (require-not (regex #\"^/dev/(tty|pty)[A-Za-z0-9]+$\"))\n")
		const tempParam = "DARWIN_RUNTIME_TMPDIR"
		params = append(params, seatbeltProfileParam{name: tempParam, path: runtimeTempDir})
		profile.WriteString("    (require-not (literal (param \"")
		profile.WriteString(tempParam)
		profile.WriteString("\")))\n")
		profile.WriteString("    (require-not (subpath (param \"")
		profile.WriteString(tempParam)
		profile.WriteString("\")))\n")
	}

	for index, exception := range exceptions {
		exceptionName := fmt.Sprintf("%s_REOPEN_%d", name, index)
		params = append(params, seatbeltProfileParam{
			name: exceptionName,
			path: nodes[exception].path,
		})
		profile.WriteString("    (require-not (literal (param \"")
		profile.WriteString(exceptionName)
		profile.WriteString("\")))\n")
		profile.WriteString("    (require-not (subpath (param \"")
		profile.WriteString(exceptionName)
		profile.WriteString("\")))\n")
	}
	profile.WriteString("  ))\n")
	return params
}

// appendSeatbeltUnixConnectDenyRule gives every hidden region the same
// boundary for AF_UNIX connect as it has for file reads. Seatbelt evaluates
// connect(2) as network-outbound, not file-read*, so omitting this sibling
// would leave a hidden socket usable. Reuse the read rule's exact parameters
// and descendant exceptions: an agentd socket reopened beneath an ordinary
// ancestor hide must remain connectable, while class-4 tmux has no reopen.
func appendSeatbeltUnixConnectDenyRule(
	profile *strings.Builder,
	name string,
	exceptions []int,
	nodes []seatbeltRegionNode,
) {
	profile.WriteString("\n(deny network-outbound\n")
	profile.WriteString("  (remote unix-socket\n")
	profile.WriteString("    (require-all\n")
	profile.WriteString("      (require-any (literal (param \"")
	profile.WriteString(name)
	profile.WriteString("\")) (subpath (param \"")
	profile.WriteString(name)
	profile.WriteString("\")))\n")
	for index := range exceptions {
		exceptionName := fmt.Sprintf("%s_REOPEN_%d", name, index)
		profile.WriteString("      (require-not (literal (param \"")
		profile.WriteString(exceptionName)
		profile.WriteString("\")))\n")
		profile.WriteString("      (require-not (subpath (param \"")
		profile.WriteString(exceptionName)
		profile.WriteString("\")))\n")
	}
	profile.WriteString("    )))\n")
}
