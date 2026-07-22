package sandboxpolicy

import "sort"

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
func ReopensUnderDeny(grants []FilesystemGrant) []ReopenUnderDeny {
	var out []ReopenUnderDeny
	for _, deny := range grants {
		if deny.Access != AccessDeny {
			continue
		}
		for _, reopen := range grants {
			if reopen.Access == AccessDeny || reopen.Path == deny.Path {
				continue
			}
			if pathContainsOrEqual(deny.Path, reopen.Path) {
				out = append(out, ReopenUnderDeny{Deny: deny.Path, Reopen: reopen})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Deny != out[j].Deny {
			return out[i].Deny < out[j].Deny
		}
		return out[i].Reopen.Path < out[j].Reopen.Path
	})
	return out
}

// HasReopenUnderDeny reports whether a grant set contains the shape at all.
// Launch and spawn gates use it to decide whether the harness capability check
// is required for this profile.
func HasReopenUnderDeny(grants []FilesystemGrant) bool {
	return len(ReopensUnderDeny(grants)) > 0
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
func EffectiveAccessAt(grants []FilesystemGrant, path string) (Access, bool) {
	best := ""
	var access Access
	found := false
	for _, grant := range grants {
		if !pathContainsOrEqual(grant.Path, path) {
			continue
		}
		// Longer canonical path == more specific. On an exact-length tie the
		// paths are equal, and normalization already folded those with deny
		// dominating write dominating read.
		if !found || len(grant.Path) > len(best) ||
			(len(grant.Path) == len(best) && accessRank(grant.Access) > accessRank(access)) {
			best = grant.Path
			access = grant.Access
			found = true
		}
	}
	return access, found
}
