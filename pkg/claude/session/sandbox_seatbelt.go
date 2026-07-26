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
	path   string
	mode   sandboxpolicy.MountMode
	policy bool
}

type seatbeltRegionNode struct {
	seatbeltRegion
	parent   int
	children []int
}

// renderSeatbeltProfile compiles the ordered mount contract into deny-only
// Seatbelt regions. Seatbelt denies dominate allows, so replaying an ancestor
// deny followed by a descendant allow would not implement MountPlan order.
// Instead this compiler first resolves the four precedence classes, then
// carves every final positive descendant out of the deny predicate that covers
// its ancestor.
//
// runtimeTempDir is the canonical Darwin $TMPDIR. Its write carveout, plus the
// fixed /dev runtime carveouts, pierces only the class-1 root write deny. A
// narrower profile RO/hide still produces its own deny without those
// exceptions, so runtime compatibility can never reopen an operator policy.
func renderSeatbeltProfile(
	phase0WriteDirs, requiredReadPaths, breakGlassPaths []string,
	plan sandboxpolicy.MountPlan,
	protectedRoots []string,
	tmuxSocketDir, runtimeTempDir string,
	identity seatbeltIdentityLookup,
) (string, []seatbeltProfileParam, error) {
	if plan.NetworkPosture != sandboxpolicy.NetworkHostOpen {
		return "", nil, fmt.Errorf(
			"darwin tclaude-layer supports only host-open networking; network posture %s "+
				"requires network/PID isolation, a constructed root, and per-socket allowlisting",
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
	requiredRead, err := cleanSeatbeltPaths("required read", requiredReadPaths)
	if err != nil {
		return "", nil, err
	}
	for _, path := range requiredRead {
		ordered = append(ordered, seatbeltRegion{path: path, mode: sandboxpolicy.MountRO})
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
			for _, readPath := range requiredRead {
				if !seatbeltSamePath(path, readPath, identity) &&
					seatbeltPathContains(path, readPath, identity) {
					ordered = append(ordered, seatbeltRegion{
						path: readPath,
						mode: sandboxpolicy.MountRO,
					})
					ordered = appendSeatbeltProtectedRehides(
						ordered,
						readPath,
						protected,
						identity,
					)
				}
			}
		}
	}

	// Class 4 is last and receives no carveout, including break-glass.
	ordered = append(ordered, seatbeltRegion{
		path: tmuxSocketDir,
		mode: sandboxpolicy.MountHide,
	})

	ordered, err = expandSeatbeltAliasRegions(ordered, plan.Aliases)
	if err != nil {
		return "", nil, err
	}
	nodes := buildSeatbeltRegionTree(ordered, identity)
	profile, params := compileSeatbeltDenyRegions(nodes, runtimeTempDir)
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
				path:   path,
				mode:   region.mode,
				policy: region.policy,
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
) (string, []seatbeltProfileParam) {
	var profile strings.Builder
	profile.WriteString("(version 1)\n")
	profile.WriteString("(allow default)\n\n")
	profile.WriteString("; Filesystem policy is deny-only. Positive descendants are carved out\n")
	profile.WriteString("; inside each deny predicate because a Seatbelt deny cannot be reopened\n")
	profile.WriteString("; by a later allow rule.\n")

	params := []seatbeltProfileParam{}
	writeRules := seatbeltDenyStarts(nodes, func(mode sandboxpolicy.MountMode) bool {
		return mode != sandboxpolicy.MountRW
	})
	normalWriteRule := make(map[int]bool, len(writeRules))
	for index, nodeIndex := range writeRules {
		normalWriteRule[nodeIndex] = true
		exceptions := seatbeltFirstAllowedDescendants(
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
		exceptions := seatbeltFirstAllowedDescendants(
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
	}
	return profile.String(), params
}

func seatbeltRuntimePolicyDenies(
	nodes []seatbeltRegionNode,
	runtimeTempDir string,
	normal map[int]bool,
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
		if seatbeltRuntimeCarveoutIntersects(node.path, runtimeTempDir) {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return nodes[out[i]].path < nodes[out[j]].path
	})
	return out
}

func seatbeltRuntimeCarveoutIntersects(path, runtimeTempDir string) bool {
	for _, carveout := range []string{"/dev", runtimeTempDir} {
		if sandboxpolicy.PathContainsOrEqual(path, carveout) ||
			sandboxpolicy.PathContainsOrEqual(carveout, path) {
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
		if node.parent >= 0 && denied(nodes[node.parent].mode) {
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
