package agentd

import (
	"encoding/json"
	"fmt"
	"sort"
)

// The scoped "always allow" — the approval popup's narrow twin of the
// existing "Always allow for this agent" button (JOH-367).
//
// The unscoped button persists a grant that covers EVERY future call of the
// slug. That is the right shape for a slug with no typed dimensions
// (human.clipboard: there is nothing to narrow it to), and the wrong shape
// the moment a slug carries dimensions — a human who wanted to stop being
// asked about ONE group should not have to hand over every group to get it.
//
// The scoped variant persists the same allow override with the scope of the
// action that is being approved, so the grant stops the popup for exactly
// that context and nothing wider. It is therefore strictly narrower than the
// unscoped button and rides the same eligibility gate.
//
// WHERE THE OFFERED SCOPE COMES FROM. It is derived from the ActionContext
// the gate site itself supplied — the same value the gate will later evaluate
// the persisted scope against — and never from re-parsing the URL or body.
// That equality is the whole point: a scope built from anything else could
// name a context the gate does not describe, and permissionScopeSatisfied
// fails closed on an undescribed dimension, so the human would click "always
// allow for this group" and keep getting popups forever. A gate site that
// passes no context (the ~129 that predate scopes) therefore offers only the
// unscoped button, which still works exactly as before.

// approvalScopeForSlug builds the scope the popup may offer to persist for a
// pending request: for each dimension the slug DECLARES, the concrete value
// the gate site described, when it described one.
//
// Returns ("", "") when there is nothing to offer — the slug declares no
// dimensions, or the gate site described none of the ones it declares. The
// caller renders no scoped button in that case.
//
// The returned JSON is canonical and has already passed the same validation
// the HTTP writers apply, so the persist path stores exactly what an operator
// could have written by hand in the permission editor.
func approvalScopeForSlug(slug string, actx ActionContext) (scopeJSON, display string) {
	dims := permissionScopeDimsForSlug(slug)
	if len(dims) == 0 {
		return "", ""
	}
	scope := PermissionScope{}
	for _, dim := range dims {
		if value := actx.value(dim); value != "" {
			scope[dim] = []string{value}
		}
	}
	if len(scope) == 0 {
		return "", ""
	}
	raw, err := json.Marshal(scope)
	if err != nil {
		return "", ""
	}
	canonical, err := canonicalPermissionScopeForSlug(slug, string(raw))
	if err != nil {
		// Defensive: the values came from a gate site, not from a caller, so
		// this means the daemon described its own action in a form the scope
		// writer rejects (a control character in a group name, say). Offering
		// no scoped button is the fail-closed answer.
		return "", ""
	}
	return canonical, permissionScopeDisplay(scope)
}

// isScopedAutoGrantableSlug gates the scoped "always allow" button and its
// server-side persist.
//
// It is deliberately the SAME allowlist as the unscoped button: a scoped
// grant is a strict narrowing of the unscoped one, so anything the popup may
// already persist for every context it may also persist for one. It is a
// separate function because the two are separate policy questions — a slug
// too sharp to hand over wholesale from one click can be perfectly reasonable
// to hand over for a single group or template — and widening the scoped set
// alone is then a one-line change here rather than a hunt through the popup.
func isScopedAutoGrantableSlug(slug string) bool {
	return IsAutoGrantableSlug(slug)
}

// mergeApprovalScope folds a newly-approved scope into the scope already
// stored on an agent's grant row for the same slug, and reports whether the
// result may be written.
//
// Without it, a second scoped always-allow would REPLACE the first: an agent
// already allowed for group "a" that gets approved for group "b" would
// silently lose "a", and the human — who only ever added permission — would
// have taken some away.
//
// But one stored scope is a CONJUNCTION of dimensions, and the union of two
// conjunctions is not one. Widening each dimension independently would author
// combinations nobody approved: {group=alpha, profile=read-only} merged with
// {group=beta, profile=full} would admit beta WITH full — the cross product,
// not the two approvals. It can also narrow: folding {profile=p1} into
// {group=alpha} constrains a previously unconstrained dimension, silently
// dropping an approval the human already gave.
//
// So the fold is allowed only where it is exactly a union: the two scopes
// constrain the SAME dimensions and differ in at most one of them. Anything
// else answers ok=false, and the caller keeps the stored scope untouched —
// the pending action is still approved, only the "stop asking me" part is
// declined, which is the sole direction that cannot invent authority.
func mergeApprovalScope(slug, existingJSON, newJSON string) (string, bool) {
	existing, err := permissionScopeForEval(existingJSON)
	if err != nil || len(existing) == 0 {
		return newJSON, true
	}
	added, err := permissionScopeForEval(newJSON)
	if err != nil || len(added) == 0 {
		return newJSON, true
	}
	if len(existing) != len(added) {
		return "", false
	}
	differing := 0
	for dim, values := range added {
		stored, ok := existing[dim]
		if !ok {
			return "", false
		}
		if !sameMatchers(stored, values) {
			differing++
		}
	}
	if differing > 1 {
		return "", false
	}
	union := PermissionScope{}
	for dim, values := range existing {
		merged := append([]string(nil), values...)
		seen := map[string]bool{}
		for _, v := range merged {
			seen[v] = true
		}
		for _, v := range added[dim] {
			if !seen[v] {
				merged = append(merged, v)
				seen[v] = true
			}
		}
		sort.Strings(merged)
		union[dim] = merged
	}
	raw, err := json.Marshal(union)
	if err != nil {
		return "", false
	}
	canonical, err := canonicalPermissionScopeForSlug(slug, string(raw))
	if err != nil {
		return "", false
	}
	return canonical, true
}

// sameMatchers compares two canonical (sorted, deduped) matcher lists.
func sameMatchers(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// errScopedAlwaysUnavailable is returned to a decision POST that asks for the
// scoped persist on a request that has no scope to persist.
var errScopedAlwaysUnavailable = fmt.Errorf("this request has no action scope to grant")
