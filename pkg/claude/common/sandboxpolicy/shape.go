package sandboxpolicy

import (
	"sort"
	"strings"
)

// This file holds the shape vocabulary that replaced the dedicated
// read-baseline/exclusion mechanism (TCL-623). Strictness is now composed from
// ordinary filesystem rows — a broad deny plus narrower read/write reopens —
// so the safety machinery keys on the SHAPE of the effective grant set rather
// than on a separate mode field.

// ReopenUnderDeny is one read/write grant that sits strictly beneath a deny
// grant in the same effective grant set. It is the shape that needs a harness
// capability gate: an unqualified deny is enforceable everywhere, but reopening
// a narrower path beneath it relies on documented path-specificity semantics
// that only some harness/mode combinations actually provide.
type ReopenUnderDeny struct {
	// Deny is the covering deny grant's canonical path.
	Deny string
	// Reopen is the narrower read/write grant beneath it.
	Reopen FilesystemGrant
}

// ReopensUnderDeny returns every reopen-under-deny pair in a grant set, sorted
// for deterministic messages. "Strictly beneath" is deliberate: a read/write
// grant at the SAME canonical path as a deny cannot exist (normalization folds
// duplicates with deny dominating), and a grant ABOVE a deny is an ordinary
// broad grant that the deny narrows rather than a carve-out.
//
// Containment is evaluated in GUEST-path space (TCL-866): the shape this gates
// is "what an agent can still reach beneath a path it was denied", which is a
// question about the sandbox namespace. A deny always occupies its host path,
// and an unremapped grant's guest path IS its host path, so this is unchanged
// for every rule set authored before mount paths existed.
func ReopensUnderDeny(grants []FilesystemGrant) []ReopenUnderDeny {
	var out []ReopenUnderDeny
	for _, deny := range grants {
		if deny.Access != AccessDeny {
			continue
		}
		for _, reopen := range grants {
			if reopen.Access == AccessDeny || reopen.GuestPath() == deny.GuestPath() {
				continue
			}
			if pathContainsOrEqual(deny.GuestPath(), reopen.GuestPath()) {
				out = append(out, ReopenUnderDeny{Deny: deny.GuestPath(), Reopen: reopen})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Deny != out[j].Deny {
			return out[i].Deny < out[j].Deny
		}
		return out[i].Reopen.GuestPath() < out[j].Reopen.GuestPath()
	})
	return out
}

// HasReopenUnderDeny reports whether a grant set contains the shape at all.
// Launch and spawn gates use it to decide whether the harness capability check
// is required for this profile.
func HasReopenUnderDeny(grants []FilesystemGrant) bool {
	return len(ReopensUnderDeny(grants)) > 0
}

// GrantsFromDirs rebuilds a grant set from the rendered launch dir lists.
//
// It exists because the shape that needs a harness capability is a property of
// what tclaude will ACTUALLY emit, not of what the operator authored. The
// launch contract adds its own reopens beneath a deny (the workspace, Git admin
// paths, agent-owned directories), so a profile whose authored rows contain no
// reopen at all — `deny ~` on its own, exactly what the "Deny access to the
// Home directory" common rule inserts — still renders as a split policy. Gating
// on the authored rows alone would let that launch skip the Codex split-policy
// probe and the macOS refusal.
//
// Duplicate paths fold with deny dominating write dominating read, matching
// normalization, so the result is directly comparable to an effective set.
func GrantsFromDirs(readDirs, writeDirs, denyDirs []string) []FilesystemGrant {
	byPath := map[string]Access{}
	add := func(paths []string, access Access) {
		for _, path := range paths {
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			if previous, exists := byPath[path]; !exists || accessRank(access) > accessRank(previous) {
				byPath[path] = access
			}
		}
	}
	add(readDirs, AccessRead)
	add(writeDirs, AccessWrite)
	add(denyDirs, AccessDeny)
	out := make([]FilesystemGrant, 0, len(byPath))
	for path, access := range byPath {
		out = append(out, FilesystemGrant{Path: path, Access: access})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// EffectiveHostAccessAt is EffectiveAccessAt asked on the HOST side: it matches
// rules by the host directory whose contents they expose, regardless of where
// inside the sandbox that content lands.
//
// The two questions come apart only once a rule carries a mount path, and they
// are both real. "What can the agent reach at /data" is the namespace question
// EffectiveAccessAt answers. "Which host directory's contents does this policy
// confer authority over" is this one, and it is what a containment check between
// a parent and a child policy must ask — otherwise a child could mount an
// arbitrary host directory at a sandbox path its parent happened to grant, and
// gain host authority the parent never had.
func EffectiveHostAccessAt(grants []FilesystemGrant, path string) (Access, bool) {
	best := ""
	var access Access
	found := false
	for _, grant := range grants {
		if !pathContainsOrEqual(grant.Path, path) {
			continue
		}
		if !found || len(grant.Path) > len(best) ||
			(len(grant.Path) == len(best) && accessRank(grant.Access) > accessRank(access)) {
			best = grant.Path
			access = grant.Access
			found = true
		}
	}
	return access, found
}

// EffectiveAccessAt reports the access a grant set actually confers at one
// canonical path, applying the most-specific-rule-wins semantics both supported
// harnesses implement (Claude Code documents it for read rules; Codex renders
// the same lattice through its permission profile). ok is false when no rule
// covers the path at all, which means the harness baseline decides.
//
// This is the read model that makes deny rows containment-checkable: without
// it, a deny and a broader read on an ancestor are indistinguishable from a
// plain read.
//
// path is a SANDBOX path and rules are matched by their guest paths, so a
// remapped grant answers for the location the agent actually sees. Callers
// asking "what does the HOST path P end up as" must not use this for a remapped
// set; that is a different question, and today's callers all ask about the
// namespace.
func EffectiveAccessAt(grants []FilesystemGrant, path string) (Access, bool) {
	best := ""
	var access Access
	found := false
	for _, grant := range grants {
		guest := grant.GuestPath()
		if !pathContainsOrEqual(guest, path) {
			continue
		}
		// Longer canonical path == more specific. On an exact-length tie the
		// paths are equal, and normalization already folded those with deny
		// dominating write dominating read.
		if !found || len(guest) > len(best) ||
			(len(guest) == len(best) && accessRank(grant.Access) > accessRank(access)) {
			best = guest
			access = grant.Access
			found = true
		}
	}
	return access, found
}
