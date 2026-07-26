package sandboxpolicy

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// This file is the authoritative renderer from an effective sandbox policy to
// the harness-neutral MountPlan IR (TCL-751). It exists because the per-harness
// renderers cannot express the operator's rule faithfully: Claude Code's
// permission layer is strict deny-first, so a deny with a narrower carve-out is
// structurally unrepresentable there, and Codex's Linux bubblewrap enforcement
// silently drops read-only carve-outs inside a denied parent. An OS-level mount
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

// RenderMountPlan renders one resolved effective sandbox policy to a MountPlan.
//
// Both filesystem authority sections contribute: the ordinary Filesystem grants
// and the acknowledged BreakGlassFilesystem grants. Break-glass rules must
// participate, or the plan would silently drop authority the operator
// explicitly acknowledged, and the resulting namespace would not match the
// policy the dashboard and audit surfaces report. Because break-glass paths sit
// at or beneath a protected root, and any deny covering that root is an
// ancestor of them, most-specific-wins already lets the acknowledged rule
// reopen the narrower path without any special case here.
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
	grants := make([]FilesystemGrant, 0, len(effective.Filesystem)+len(effective.BreakGlassFilesystem))
	grants = append(grants, effective.Filesystem...)
	for i, grant := range effective.BreakGlassFilesystem {
		// Break-glass never denies: deny is already the default for protected
		// roots, so a deny here would mean the value was never normalized.
		if grant.Access != AccessRead && grant.Access != AccessWrite {
			return MountPlan{}, fmt.Errorf(
				"break_glass_filesystem[%d].access %q is invalid (want read or write)", i, grant.Access)
		}
		grants = append(grants, FilesystemGrant{Path: grant.Path, Access: grant.Access})
	}
	return RenderMountPlanFromGrants(grants)
}

// RenderMountPlanFromGrants is the primitive RenderMountPlan is built on. It
// renders any effective grant set, including one assembled by the launch
// contract through GrantsFromDirs, whose rows come from rendered dir lists
// rather than from a normalized profile.
//
// # Ordering
//
// Entries are sorted lexically by canonical path. That is not a cosmetic
// choice: for cleaned absolute paths an ancestor is always a proper string
// prefix of its descendants, so lexical order IS ancestors-first, and it is
// total, so identical inputs always produce byte-identical plans. Unrelated
// siblings may interleave between an ancestor and its descendant, which is
// harmless — shadowing only ever happens between entries that actually contain
// one another.
//
// Every authored path is emitted, including one whose mode equals what it would
// already inherit from an ancestor. Re-binding a subtree at the mode it already
// has is a no-op for the applier, and keeping the row means an operator reading
// a dry-run plan sees every rule they wrote instead of wondering where it went.
//
// # Path-handling policy (TCL-751 decision)
//
// Symlinks: resolved BEFORE this renderer, never inside it. Normalize and
// Resolve already fully EvalSymlinks every path and re-resolve the merged set,
// so the grants arriving here name real targets. Re-resolving would make the
// renderer impure and non-deterministic, and would reopen the very
// time-of-check window Resolve closes. The honest consequence, which the
// applier owns: the plan binds the RESOLVED target, so an aliased spelling of
// that path (an unmounted symlink pointing at it) does not itself exist inside
// the namespace. Materializing alias paths is a launch-contract concern, not a
// policy one.
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
// Invalid input is rejected rather than dropped. Silently discarding a
// malformed row would fail OPEN whenever that row was a deny.
func RenderMountPlanFromGrants(grants []FilesystemGrant) (MountPlan, error) {
	byPath := make(map[string]Access, len(grants))
	for i, grant := range grants {
		switch grant.Access {
		case AccessRead, AccessWrite, AccessDeny:
		default:
			return MountPlan{}, fmt.Errorf(
				"mount plan grant %d (%q): access %q is invalid (want read, write, or deny)", i, grant.Path, grant.Access)
		}
		path := strings.TrimSpace(grant.Path)
		if path == "" {
			return MountPlan{}, fmt.Errorf("mount plan grant %d: path is required", i)
		}
		if !filepath.IsAbs(path) {
			return MountPlan{}, fmt.Errorf(
				"mount plan grant %d: path %q is not absolute; render from resolved policy paths", i, path)
		}
		path = filepath.Clean(path)
		// Same-path folding uses the package-wide lattice so a plan composed
		// from several sections cannot depend on the order they were appended.
		if previous, exists := byPath[path]; !exists || accessRank(grant.Access) > accessRank(previous) {
			byPath[path] = grant.Access
		}
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	entries := make([]MountEntry, 0, len(paths))
	for _, path := range paths {
		entries = append(entries, MountEntry{Path: path, Mode: mountModeForAccess(byPath[path])})
	}
	return MountPlan{Entries: entries}, nil
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
