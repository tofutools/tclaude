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
// That makes them a contract on the applier, not an oversight, and the contract
// has an ORDER requirement — three phases, in which placement is what carries
// the meaning:
//
//  1. Hide ProtectedPaths() BEFORE replaying these entries. Break-glass grants
//     then reopen their narrower paths by the same most-specific-wins ordering
//     as everything else. An applier that replayed the plan first and applied
//     this baseline afterwards would silently revoke the operator's
//     acknowledged break-glass authority; one that omitted it entirely would
//     expose tclaude's own private state to the agent.
//  2. Replay the plan.
//  3. Hide the strictly-unreachable class AFTER replaying — today the tmux
//     socket directory, which the Codex adapter already treats as host-control
//     authority, a more severe class than protected state and deliberately not
//     reachable through break-glass. It must come last precisely BECAUSE it is
//     not in ProtectedPaths(): an ordinary rw row at that path passes profile
//     validation, since it intersects no protected root, so a before-plan hide
//     could be shadowed by an innocent-looking grant. Applying it after the
//     replay is what encodes "not reachable through break-glass, or anything
//     else" — most-specific-wins governs the policy, and this class sits
//     outside the policy rather than at the top of it.
//
// (Phase 3 per the TCL-750 applier ruling; the renderer emits nothing for
// either phase 1 or phase 3.)
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
// Both filesystem authority sections contribute: the ordinary Filesystem grants
// and the acknowledged BreakGlassFilesystem grants. Break-glass rules must
// participate, or the plan would silently drop authority the operator
// explicitly acknowledged, and the resulting namespace would not match the
// policy the dashboard and audit surfaces report. Since a break-glass path sits
// at or beneath a protected root, and the applier's baseline deny over that
// root is established first (see "What the plan does NOT contain" above),
// most-specific-wins reopens the narrower path with no special case here.
//
// The one collision that needs a rule is a deny and a break-glass grant on the
// SAME canonical path. That folds the way every other same-path collision in
// this package folds — deny dominates write dominates read — so composition
// stays fail-closed and order-independent. An operator who wants the carve-out
// authors it strictly beneath the deny, which is the shape the whole model is
// built around.
//
// Sections that do not describe host paths (Environment, AgentDirectories,
// NetworkAccess) are deliberately absent: AgentDirectories are private
// directories agentd mints per agent rather than host authority the profile
// grants, and the network posture is not a mount. They belong to the launch
// contract that consumes this plan.
func RenderMountPlan(effective EffectiveProfile) (MountPlan, error) {
	breakGlass := make([]FilesystemGrant, 0, len(effective.BreakGlassFilesystem))
	for i, grant := range effective.BreakGlassFilesystem {
		// Break-glass never denies: deny is already the default for protected
		// roots, so a deny here would mean the value was never normalized.
		if grant.Access != AccessRead && grant.Access != AccessWrite {
			return MountPlan{}, fmt.Errorf(
				"break_glass_filesystem[%d].access %q is invalid (want read or write)", i, grant.Access)
		}
		// A direct conversion rather than a field-by-field literal: if the two
		// grant shapes ever diverge this stops compiling, which is exactly the
		// moment to decide what the new field means for the mount plan, instead
		// of silently dropping it here.
		breakGlass = append(breakGlass, FilesystemGrant(grant))
	}
	return renderMountPlanSections([]mountGrantSection{
		{label: "filesystem", grants: effective.Filesystem},
		{label: "break_glass_filesystem", grants: breakGlass},
	})
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
// it) does not itself exist inside the namespace. Materializing alias paths is
// a launch-contract concern, not a policy one.
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
	byPath := make(map[string]Access, total)
	for _, section := range sections {
		for i, grant := range section.grants {
			switch grant.Access {
			case AccessRead, AccessWrite, AccessDeny:
			default:
				return MountPlan{}, fmt.Errorf("%s[%d].access %q is invalid (want read, write, or deny)",
					section.label, i, grant.Access)
			}
			path, err := canonicalMountPath(grant.Path)
			if err != nil {
				return MountPlan{}, fmt.Errorf("%s[%d].path: %w", section.label, i, err)
			}
			// Same-path folding uses the package-wide lattice so a plan composed
			// from several sections cannot depend on the order they were appended.
			if previous, exists := byPath[path]; !exists || accessRank(grant.Access) > accessRank(previous) {
				byPath[path] = grant.Access
			}
		}
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sortMountPaths(paths)
	entries := make([]MountEntry, 0, len(paths))
	for _, path := range paths {
		entries = append(entries, MountEntry{Path: path, Mode: mountModeForAccess(byPath[path])})
	}
	return MountPlan{Entries: entries}, nil
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
