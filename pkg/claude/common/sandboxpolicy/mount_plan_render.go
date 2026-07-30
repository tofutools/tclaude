package sandboxpolicy

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// This file renders an effective sandbox policy to the harness-neutral
// MountPlan IR (TCL-751). It exists because the per-harness renderers cannot
// express the operator's rule faithfully: Claude Code's permission layer is
// strict deny-first, so a deny with a narrower carve-out is structurally
// unrepresentable there, and Codex's Linux bubblewrap enforcement silently
// drops read-only carve-outs inside a denied parent. An OS-level mount
// namespace built by tclaude itself CAN express it, uniformly, because mounts
// compose by shadowing rather than by rule precedence.
//
// The rule this renderer implements is the epic's requirement 2: THE MOST
// SPECIFIC FILESYSTEM PATH WINS, uniformly, with no per-harness carve
// semantics. It is implemented by construction rather than by evaluation —
// entries are emitted ancestors-first, so an applier that walks the plan in
// order and lets each entry shadow whatever came before lands on exactly the
// most-specific rule for every path. No applier needs to understand policy
// precedence; it only needs to preserve order.
//
// The renderer is pure: it performs no filesystem access at all. Everything it
// needs about the real filesystem (symlink resolution, directory-ness,
// protected-root containment) has already been decided by Normalize/Resolve,
// which are the layer that owns those questions. See the path-handling policy
// notes on RenderMountPlanFromGrants.
//
// # What the plan does NOT contain
//
// The plan carries the POLICY's own authority and nothing else. In particular
// it does not carry the protected-root baseline — the denies over exactly
// ProtectedPaths(), that is ~/.tclaude/data and ~/.claude/sessions, which
// today's harness adapters inject at their own rendering seams. Those are not
// part of EffectiveProfile, so they cannot appear here, and deriving them would
// require the filesystem access this renderer deliberately does without.
//
// ~/.tclaude/api is NOT part of that baseline and must stay visible: it is the
// agent-reachable directory holding the agentd control socket (see
// common.TclaudeAPIDir), and hiding it would cut the agent off from
// coordination. Only the private state under ~/.tclaude/data is protected. The
// distinction matters here because per-socket allowlisting — binding exactly
// that socket and nothing else — is the capability this IR is meant to grow
// into.
//
// That makes them a contract on the applier, not an oversight. Four precedence
// classes carry the meaning; they are not merely a literal mount sequence:
//
//  1. Launch-contract binds keep the harness state, workspace/Git
//     administration and agent directories writable. They survive an ordinary
//     plan deny on an ancestor (an applier may repair the narrower bind after
//     applying that deny), but lose to protected-root hides. An ordinary rule
//     at or below the harness state root must be refused rather than silently
//     launching a harness that cannot persist.
//  2. Plan entries replay exactly in order. Most-specific-wins remains the
//     policy rule.
//  3. ProtectedPaths() hides beat launch-contract repairs and every ordinary
//     rule. They are established before replay and restored after any repair,
//     and NOTHING reopens beneath them. That is unconditional since TCL-791
//     removed break-glass, the one former exception: no profile, include,
//     launch contract, acknowledgement, or flag can carve a path back out of a
//     protected root. An operator who must work without the wall disables the
//     sandbox instead.
//  4. The strictly-unreachable class — today the tmux socket directory — beats
//     everything. It must come last precisely BECAUSE it is not in
//     ProtectedPaths(): an ordinary rw row at that path passes profile
//     validation, so a before-plan hide could be shadowed by an
//     innocent-looking grant.
//
// The renderer emits only class 2. The other classes belong to the launch
// contract and applier.
//
// # How the plan grows (settled TCL-751 decision, epic requirement 3)
//
// Per-socket unix-socket allowlists — the capability that retires
// allowAllUnixSockets — need no IR change at all: allowing one socket is an
// ordinary read-only bind of that socket file's path, so it rides the existing
// {Path, Mode} entry as-is. Such entries come from the launch contract rather
// than from a profile, because profile paths are directory-only
// (canonicalDirectory requires IsDir).
//
// A genuinely non-filesystem resource class (network posture, seccomp rules)
// gets a SIBLING FIELD on MountPlan instead of a kind discriminator on
// MountEntry. That keeps the entry type and every existing consumer stable, and
// keeps "is this a mount?" from becoming a runtime question in appliers that
// only ever wanted the ordered path list.

// mountGrantSection is one labelled run of grants. The label is the operator's
// own name for the profile field the rows came from, so an error can point at
// the row the operator actually wrote instead of at an index into whatever
// internal concatenation the renderer happened to build.
type mountGrantSection struct {
	label  string
	grants []FilesystemGrant
}

// RenderMountPlan renders one resolved effective sandbox policy to a MountPlan.
//
// Only the ordinary Filesystem grants contribute. Every one of them has already
// been proven by normalizeFilesystem not to intersect a protected root, so no
// entry this renderer emits can land at or beneath one — the class-3 hides
// above are never reopened. There is deliberately no second authority section:
// break_glass_filesystem was the one exception and TCL-791 removed it.
//
// Sections that do not describe host paths (Environment and AgentDirectories)
// are deliberately absent: AgentDirectories are private directories agentd
// mints per agent rather than host authority the profile grants. NetworkAccess
// maps onto the sibling NetworkPosture field rather than onto Entries.
func RenderMountPlan(effective EffectiveProfile) (MountPlan, error) {
	axes, err := PlannedEffectiveAccessAxes(effective)
	if err != nil {
		return MountPlan{}, err
	}
	posture, err := NetworkPostureForRules(axes.Network)
	if err != nil {
		return MountPlan{}, err
	}
	plan, err := renderMountPlanSections([]mountGrantSection{
		{label: "filesystem", grants: effective.Filesystem},
	})
	if err != nil {
		return MountPlan{}, err
	}
	plan.Aliases, err = renderMountAliases(effective.MountAliases)
	if err != nil {
		return MountPlan{}, err
	}
	plan.NetworkPosture = posture
	if posture == NetworkFiltered {
		filtered, filteredErr := CompileFilteredNetworkRules(axes.Network)
		if filteredErr != nil {
			return MountPlan{}, fmt.Errorf("compile filtered network plan: %w", filteredErr)
		}
		plan.FilteredNetwork = &filtered
	}
	return plan, nil
}

// NetworkPostureForRules maps the new access axis onto the reserved mount-plan
// seam. Capability planning must widen an unenforceable list before an applier
// sees it; a surviving NetworkFiltered value is therefore an enforceable-or-
// refuse contract, never permission to silently approximate.
func NetworkPostureForRules(rules NetworkRules) (NetworkPosture, error) {
	switch rules.Mode {
	case AccessModeUnset:
		return NetworkHostOpen, nil
	case AccessModeOpen:
		if len(rules.Deny) > 0 {
			return NetworkFiltered, nil
		}
		return NetworkHostOpen, nil
	case AccessModeClosed:
		return NetworkIsolatedWithAgentd, nil
	case AccessModeList:
		return NetworkFiltered, nil
	default:
		return NetworkHostOpen, fmt.Errorf(
			"network.mode %q is invalid (want open, closed, list, or omitted)",
			rules.Mode,
		)
	}
}

// NetworkPostureForAccess maps the operator-authored network intent onto the
// OS-layer IR. Inherit preserves the walking skeleton's host namespace;
// reinterpreting an omitted field as isolated would strand every hosted model
// client now that the outer layer wraps the whole harness process.
func NetworkPostureForAccess(access NetworkAccess) (NetworkPosture, error) {
	switch access {
	case NetworkAccessInherit, NetworkAccessInternet:
		return NetworkHostOpen, nil
	case NetworkAccessNone:
		return NetworkIsolatedWithAgentd, nil
	default:
		return NetworkHostOpen, fmt.Errorf(
			"network_access %q is invalid (want internet, none, or omitted)", access)
	}
}

// RenderMountPlanFromGrants is the primitive RenderMountPlan is built on. It
// renders any effective grant set, including one assembled by the launch
// contract through GrantsFromDirs, whose rows come from rendered dir lists
// rather than from a normalized profile.
//
// # Ordering
//
// Entries are sorted by a case-folded, NFC-normalized form of the canonical
// path, with the exact path as the tie-break so the order stays total and the
// plan stays byte-identical for identical inputs.
//
// The folding is what makes ancestors-first hold on the platforms this project
// targets. For cleaned absolute paths a byte-exact ancestor is always a proper
// string prefix of its descendants, and both folding steps distribute over the
// "/" join, so plain lexical order would already be ancestors-first for
// byte-identical spellings. It is not enough on macOS, a declared Seatbelt
// target, where the filesystem is case-insensitive and normalization-
// insensitive: "/Users/dev" and "/users/dev" are ONE directory there, yet
// byte-wise they are unrelated strings, and a descendant spelled in NFD sorts
// BEFORE an ancestor spelled in NFC. Either would let a later allow re-expose a
// path an earlier deny was meant to hide. Folding puts such spellings back into
// the containment order the kernel will actually enforce, and costs nothing on
// a case-sensitive filesystem, where the two spellings really are unrelated
// directories whose relative order never mattered.
//
// Folding orders those spellings; it does not MERGE them. Two spellings of one
// macOS directory remain two entries, and only the later one's mode survives.
// Collapsing them would require knowing whether the filesystem is
// case-insensitive, which is exactly the filesystem question this renderer
// refuses to ask; canonicalizing case at authoring time, where that question
// can be answered, is the durable fix and is tracked with the Seatbelt
// workstream.
//
// Every authored path is emitted, including one whose mode equals what it would
// already inherit from an ancestor. Re-binding a subtree at the mode it already
// has is a no-op for the applier, and keeping the row means an operator reading
// a dry-run plan sees every rule they wrote instead of wondering where it went.
//
// # Path-handling policy (TCL-751 decision)
//
// Symlinks: resolved BEFORE this renderer, never inside it. Re-resolving would
// make the renderer impure and non-deterministic, and would reopen the very
// time-of-check window Resolve closes. On the Resolve path this means the
// grants arriving here already name real targets; a launch-contract set built
// through GrantsFromDirs carries whatever its caller resolved, and a path that
// does not exist yet is canonicalized only as far as its longest existing
// ancestor (canonicalMissingDirectory), with the remaining suffix lexical. The
// honest consequence, which the applier owns: the plan binds the RESOLVED
// target, so an aliased spelling of that path (an unmounted symlink pointing at
// it) must be recreated separately in a constructed root. Resolve records every
// still-observable spelling as MountAliases; this renderer only validates and
// copies those pairs into the sibling MountPlan field, preserving its
// filesystem purity.
//
// Missing paths: emitted, not skipped, and never pre-created. The renderer is
// pure and so cannot know whether a path exists, but that is the right
// behavior independently, because it keeps the two directions honest:
//
//   - A missing HIDE entry costs nothing and stays correct if the path appears
//     later; applying it is always the safe direction.
//   - A missing RO/RW entry must be SKIPPED BY THE APPLIER, not pre-created.
//     Pre-creating would have tclaude mkdir on the operator's host as a side
//     effect of launching, and would hand the agent a writable empty directory
//     that confers authority over nothing real — a grant that looks satisfied
//     while the host path it named does not exist. Skipping instead matches the
//     semantics the profile model already documents for missing paths
//     (NormalizeForPersistence): the rule survives resolution and becomes
//     active on a later launch, once the directory exists and revalidates.
//
// Remapped grants (TCL-866): a grant carrying a mount_path is emitted as an
// entry whose Path is the GUEST path and whose Source is the canonical host
// path. Everything above about ordering, folding and shadowing is stated in
// guest-path space, because that is the space the applier's mount table lives
// in and the space the agent observes. Host-path authority is untouched:
// symlink resolution, directory-ness and protected-root containment were all
// decided against Path by Normalize/Resolve before the renderer ran, and the
// guest path is validated syntactically only — the renderer still performs no
// filesystem access, so it cannot and must not ask whether the guest mountpoint
// exists. That question belongs to the applier, which owns the namespace.
//
// Malformed input is rejected rather than dropped, because silently discarding
// a row would fail OPEN whenever that row was a deny. The syntactic checks
// mirror canonicalDirectory's — absolute, within MaxPathBytes, valid UTF-8, no
// control characters — so a row this renderer accepts is one the profile layer
// would also have accepted. Everything canonicalDirectory can only answer by
// touching the filesystem (existence, directory-ness, symlink identity) stays
// with that layer.
func RenderMountPlanFromGrants(grants []FilesystemGrant) (MountPlan, error) {
	return renderMountPlanSections([]mountGrantSection{{label: "filesystem", grants: grants}})
}

func renderMountPlanSections(sections []mountGrantSection) (MountPlan, error) {
	total := 0
	for _, section := range sections {
		total += len(section.grants)
	}
	type plannedMount struct {
		access Access
		source string
	}
	byGuest := make(map[string]plannedMount, total)
	for _, section := range sections {
		for i, grant := range section.grants {
			switch grant.Access {
			case AccessRead, AccessWrite, AccessDeny:
			default:
				return MountPlan{}, fmt.Errorf("%s[%d].access %q is invalid (want read, write, or deny)",
					section.label, i, grant.Access)
			}
			source, err := canonicalMountPath(grant.Path)
			if err != nil {
				return MountPlan{}, fmt.Errorf("%s[%d].path: %w", section.label, i, err)
			}
			guest := source
			if grant.MountPath != "" {
				if grant.Access == AccessDeny {
					return MountPlan{}, fmt.Errorf(
						"%s[%d].mount_path is not allowed on a deny rule", section.label, i)
				}
				guest, err = canonicalMountPath(grant.MountPath)
				if err != nil {
					return MountPlan{}, fmt.Errorf("%s[%d].mount_path: %w", section.label, i, err)
				}
			}
			// Folding happens in GUEST-path space, because that is the position
			// the entry occupies inside the namespace and therefore what a later
			// entry would shadow. Two rules projecting different host directories
			// onto one guest path are not the same rule spelled twice; folding
			// them would silently discard one authored grant, so refuse instead.
			previous, exists := byGuest[guest]
			if !exists {
				byGuest[guest] = plannedMount{access: grant.Access, source: source}
				continue
			}
			if previous.source != source {
				return MountPlan{}, fmt.Errorf(
					"%s[%d]: sandbox path %q is claimed by two different host paths %q and %q",
					section.label, i, guest, previous.source, source)
			}
			// Same-path folding uses the package-wide lattice so a plan composed
			// from several sections cannot depend on the order they were appended.
			if accessRank(grant.Access) > accessRank(previous.access) {
				previous.access = grant.Access
				byGuest[guest] = previous
			}
		}
	}
	paths := make([]string, 0, len(byGuest))
	for path := range byGuest {
		paths = append(paths, path)
	}
	sortMountPaths(paths)
	entries := make([]MountEntry, 0, len(paths))
	for _, path := range paths {
		planned := byGuest[path]
		entry := MountEntry{Path: path, Mode: mountModeForAccess(planned.access)}
		if planned.source != path {
			entry.Source = planned.source
		}
		entries = append(entries, entry)
	}
	return MountPlan{Entries: entries}, nil
}

func renderMountAliases(in []MountAlias) ([]MountAlias, error) {
	if len(in) == 0 {
		return nil, nil
	}
	byLink := make(map[string]string, len(in))
	for i, alias := range in {
		link, err := canonicalMountPath(alias.Link)
		if err != nil {
			return nil, fmt.Errorf("mount_aliases[%d].link: %w", i, err)
		}
		target, err := canonicalMountPath(alias.Target)
		if err != nil {
			return nil, fmt.Errorf("mount_aliases[%d].target: %w", i, err)
		}
		if previous, exists := byLink[link]; exists && previous != target {
			return nil, fmt.Errorf(
				"mount_aliases[%d].link %q has conflicting targets %q and %q",
				i, link, previous, target,
			)
		}
		byLink[link] = target
	}
	links := make([]string, 0, len(byLink))
	for link := range byLink {
		links = append(links, link)
	}
	sortMountPaths(links)
	out := make([]MountAlias, 0, len(links))
	for _, link := range links {
		out = append(out, MountAlias{Link: link, Target: byLink[link]})
	}
	return out, nil
}

// canonicalMountPath applies the syntactic half of canonicalDirectory: every
// check that can be made without touching the filesystem. Keeping the two in
// step means the renderer never accepts a spelling the profile layer would have
// rejected, which matters because RenderMountPlanFromGrants also takes rows the
// profile layer never saw.
func canonicalMountPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	if len(path) > MaxPathBytes {
		return "", fmt.Errorf("path is too long (maximum %d bytes)", MaxPathBytes)
	}
	// A NUL or newline would survive all the way to the exec boundary and fail
	// there as something unrecognizable; reject it where it can still be
	// attributed to a rule.
	if !utf8.ValidString(path) || strings.ContainsFunc(path, isControl) {
		return "", fmt.Errorf("path must be valid UTF-8 without control characters")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q is not absolute; render from resolved policy paths", path)
	}
	return filepath.Clean(path), nil
}

// sortMountPaths establishes the ancestors-first order. See the Ordering
// section on RenderMountPlanFromGrants for why the key is folded rather than
// the raw path.
func sortMountPaths(paths []string) {
	keys := make(map[string]string, len(paths))
	for _, path := range paths {
		keys[path] = mountOrderKey(path)
	}
	sort.Slice(paths, func(i, j int) bool {
		if keys[paths[i]] != keys[paths[j]] {
			return keys[paths[i]] < keys[paths[j]]
		}
		return paths[i] < paths[j]
	})
}

// mountOrderKey folds one path to the form used for ordering. Both steps
// distribute over the "/" join — ToLower maps rune by rune, and NFC never
// composes across a "/", which is a starter — so a byte-exact ancestor's key
// stays a proper prefix of its descendant's key. That is what keeps plain
// containment ordering intact while additionally ordering the case- and
// normalization-variant spellings that are one directory on macOS.
func mountOrderKey(path string) string {
	return norm.NFC.String(strings.ToLower(path))
}

// mountModeForAccess maps one policy access to its mount mode. It is total on
// purpose: an access value this package does not recognize renders as
// MountHide, so a future or corrupted value can never widen a sandbox. Callers
// reject unknown access before reaching here; this is the second line.
func mountModeForAccess(access Access) MountMode {
	switch access {
	case AccessRead:
		return MountRO
	case AccessWrite:
		return MountRW
	default:
		return MountHide
	}
}

// EffectiveMountModeAt replays a plan the way an applier does — walk entries in
// order, let each one that covers the path shadow whatever came before — and
// reports the mode the path ends up with. ok is false when no entry covers the
// path at all, which means the applier's baseline decides rather than the
// policy; the returned mode is MountHide in that case so a caller that ignores
// ok still fails closed.
//
// This is the executable statement of the guarantee: it evaluates nothing about
// specificity, only order, and it is what the tests use to prove the ordered
// plan agrees with EffectiveAccessAt's independent most-specific-wins model.
//
// Containment here is byte-exact, matching the rest of this package. On a
// case-insensitive filesystem the kernel's own containment is coarser, so this
// models the plan rather than the mount table; ordering already accounts for
// those spellings (see sortMountPaths).
func EffectiveMountModeAt(plan MountPlan, path string) (MountMode, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return MountHide, false
	}
	path = filepath.Clean(path)
	mode := MountHide
	found := false
	for _, entry := range plan.Entries {
		if pathContainsOrEqual(entry.Path, path) {
			mode = entry.Mode
			found = true
		}
	}
	return mode, found
}
