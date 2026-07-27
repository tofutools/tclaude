package session

import (
	"fmt"
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
func renderSeatbeltProfile(
	phase0WriteDirs, requiredAgentdSocketPaths, breakGlassPaths []string,
	plan sandboxpolicy.MountPlan,
	protectedRoots []string,
	tmuxSocketDir, runtimeTempDir string,
	identity seatbeltIdentityLookup,
	privateWriteDirs ...TclaudeLayerPrivateWriteDir,
) (string, []seatbeltProfileParam, error) {
	switch plan.NetworkPosture {
	case sandboxpolicy.NetworkHostOpen, sandboxpolicy.NetworkIsolatedWithAgentd:
	case sandboxpolicy.NetworkFiltered:
		return "", nil, fmt.Errorf(
			"darwin tclaude-layer does not support reserved filtered networking: "+
				"network posture %s requires a proxy-backed network applier",
			plan.NetworkPosture,
		)
	default:
		return "", nil, fmt.Errorf(
			"darwin tclaude-layer has invalid network posture %d",
			plan.NetworkPosture,
		)
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

	breakGlass := make([]string, 0, len(breakGlassPaths))
	for i, path := range breakGlassPaths {
		clean, cleanErr := cleanSeatbeltPath(fmt.Sprintf("break-glass path %d", i), path)
		if cleanErr != nil {
			return "", nil, cleanErr
		}
		breakGlass = append(breakGlass, clean)
	}

	// Class 1 starts from the host root read-only, then reopens the launch
	// contract. Class 3 is established before class-2 replay so acknowledged
	// break-glass entries can reopen it later.
	ordered := []seatbeltRegion{{path: string(filepath.Separator), mode: sandboxpolicy.MountRO}}
	for _, path := range contract {
		ordered = append(ordered, seatbeltRegion{path: path, mode: sandboxpolicy.MountRW})
	}
	requiredAgentdSockets, err := cleanSeatbeltPaths(
		"required agentd socket",
		requiredAgentdSocketPaths,
	)
	if err != nil {
		return "", nil, err
	}
	for _, path := range requiredAgentdSockets {
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

		isBreakGlass := seatbeltPathIn(path, breakGlass, identity)
		switch entry.Mode {
		case sandboxpolicy.MountRO, sandboxpolicy.MountRW:
			if !isBreakGlass {
				ordered = appendSeatbeltProtectedRehides(ordered, path, protected, identity)
			}
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
			for _, agentdSocket := range requiredAgentdSockets {
				if !seatbeltSamePath(path, agentdSocket, identity) &&
					seatbeltPathContains(path, agentdSocket, identity) {
					ordered = append(ordered, seatbeltRegion{
						path:             agentdSocket,
						mode:             sandboxpolicy.MountRO,
						networkException: true,
					})
					ordered = appendSeatbeltProtectedRehides(
						ordered,
						agentdSocket,
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
				// daemon-authored exception must beat any policy/break-glass
				// carveout while still reopening exactly the current child.
				denyBoundary: true,
			},
			seatbeltRegion{
				path: current, mode: sandboxpolicy.MountRW,
				daemonReopen: true,
			},
		)
	}

	// Class 4 is last and receives no carveout, including break-glass.
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
		plan.NetworkPosture,
		identity,
	)
	return profile, params, nil
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

func seatbeltPathIn(
	path string,
	candidates []string,
	identity seatbeltIdentityLookup,
) bool {
	for _, candidate := range candidates {
		if seatbeltSamePath(path, candidate, identity) {
			return true
		}
	}
	return false
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
	networkPosture sandboxpolicy.NetworkPosture,
	identity seatbeltIdentityLookup,
) (string, []seatbeltProfileParam) {
	var profile strings.Builder
	profile.WriteString("(version 1)\n")
	profile.WriteString("(allow default)\n\n")
	profile.WriteString("; Filesystem policy is deny-only. Positive descendants are carved out\n")
	profile.WriteString("; inside each deny predicate so plan precedence does not depend on\n")
	profile.WriteString("; Seatbelt allow/deny rule selection.\n")

	params := []seatbeltProfileParam{}
	if networkPosture == sandboxpolicy.NetworkIsolatedWithAgentd {
		params = appendSeatbeltIsolatedNetworkRules(&profile, params, nodes)
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

// appendSeatbeltIsolatedNetworkRules blocks every connection except connect(2)
// to the parameterized agentd Unix socket spellings, and blocks creation of
// every listener. The exception belongs inside the outbound deny predicate so
// connectivity does not depend on Seatbelt allow/deny rule selection.
//
// Do not replace these operations with network* or deny system-socket.
// Creating an AF_UNIX socket descriptor is a pathless system-socket operation,
// but socket creation is not connectivity; Linux isolated permits it too. A
// network* deny would block the descriptor agentd needs and could not be
// carved back open.
//
// network-inbound is deliberately absent. On real Darwin hardware it blocks
// agentd replies, and a remote-unix exception parses but does not reopen them.
// Listener prevention therefore rests on network-bind. A listening descriptor
// handed in over SCM_RIGHTS would require cooperation from the trusted agentd
// daemon and is outside this boundary's threat model.
func appendSeatbeltIsolatedNetworkRules(
	profile *strings.Builder,
	params []seatbeltProfileParam,
	nodes []seatbeltRegionNode,
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

	profile.WriteString("\n; Isolated networking denies host/public connectivity and listeners.\n")
	profile.WriteString("; Only agentd connects at the parameterized socket spellings are excepted.\n")
	profile.WriteString("(deny network-bind)\n")
	if len(exceptions) == 0 {
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
	appendSeatbeltNetworkDenyExceptAgentd(profile, "network-outbound", len(exceptions))
	return params
}

func appendSeatbeltNetworkDenyExceptAgentd(
	profile *strings.Builder,
	action string,
	exceptionCount int,
) {
	profile.WriteString("(deny ")
	profile.WriteString(action)
	profile.WriteString("\n")
	profile.WriteString("  (require-all\n")
	for index := range exceptionCount {
		name := fmt.Sprintf("AGENTD_SOCKET_%d", index)
		profile.WriteString("    (require-not\n")
		profile.WriteString("      (remote unix-socket\n")
		profile.WriteString("        (literal (param \"")
		profile.WriteString(name)
		profile.WriteString("\"))))\n")
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
