package agentd

import (
	"net/http"
	"slices"
	"strings"
)

// scopedGrantDimension is what differs between the proxies whose grants are
// scoped to a set of keys — AWB (awb_workspace) and Linear (linear_team): the
// scope dimension, the operator's allow-list, and the words their refusals use.
// The resolution rules themselves are one, in resolveScopedGrantKeys.
type scopedGrantDimension struct {
	dim ScopeDim
	// context describes one key to the scope evaluator.
	context func(key string) ActionContext
	// allowed is the operator's normalized allow-list; allowedKey reports
	// membership of one key in it.
	allowed    []string
	allowedKey func(key string) bool
	// disabled is the refusal for an unscoped grant with no operator list.
	disabled *proxyFault
	// emptyCode is the code of the three "the scope authorizes nothing" refusals.
	emptyCode string
	// Wording: product "AWB", noun "workspace", request "an AWB request",
	// example "awb_workspace=awb", listKey "agent.awb_proxy.allowed_workspaces".
	product, noun, request, example, listKey string
}

// resolveScopedGrantKeys resolves the set of keys one request may act within,
// from the operator's allow-list and the caller's grant scope. The rules:
//
//   - unscoped grant, operator list  → the operator's list;
//   - scoped grant, operator list    → the intersection, so an operator can
//     narrow one agent without touching the global list, and a grant can never
//     widen past it;
//   - scoped grant, no operator list → the grant's own keys, mirroring the git
//     proxy's remote-scoped grant with an empty allowed_remotes;
//   - unscoped grant, no operator list → nothing is reachable: the fail-closed
//     disabled answer.
//
// It resolves in TWO steps, and the split is what makes both answers precise.
//
// First, which keys the SCOPE admits, on its own merits: every key the scope
// names, put through the real evaluator. That is not the same as the names
// themselves. permissionScopeEnumerate unions the matchers across the winning
// tier's rows, so a tier holding {linear_team: TCL} beside {linear_team: JOH,
// group: dev} NAMES both while admitting only TCL — and reporting JOH as
// something the grant covers would over-claim in a refusal message and in the
// audit row. Only evalPermissionScope decides, which is also what keeps a scope
// carrying a second dimension ANDing rather than collapsing to its key term.
//
// Second, the operator's ceiling intersected over that — or, with no operator
// list at all, the scope's own answer standing as the whole policy.
//
// Separating the steps also puts the two ways an empty result arises on two
// different paths, so each gets the refusal that names what the operator would
// actually have to change.
//
// grantKeys is the scope's own answer (nil for an unscoped grant), returned
// even beside a no-overlap fault so a caller can still name it.
func resolveScopedGrantKeys(
	r *http.Request, convID, perm string, d scopedGrantDimension, scoped bool,
) (keys, grantKeys []string, fault *proxyFault) {
	if !scoped {
		if len(d.allowed) == 0 {
			return nil, nil, d.disabled
		}
		return d.allowed, nil, nil
	}
	// A SECOND resolution, because the preflight's verdict does not travel here.
	// It re-reads the same request-scoped defaults, so in the ordinary case this
	// is the same verdict the preflight saw.
	v := resolvePermissionVerdictForRequest(r, convID, perm)
	if v.Resolution != permAllow {
		// The grant changed between the two reads. Name THAT rather than blaming
		// the shape of a scope that may no longer exist — and refuse, rather than
		// falling back on the operator's list.
		return nil, nil, faultf(http.StatusForbidden, "permission",
			"the %s grant was withdrawn while this request was being authorized", perm)
	}
	named, ok := permissionScopeEnumerate(v, d.dim)
	if !ok || len(named) == 0 {
		// The preflight established a scoped allow, so reaching here means this
		// build cannot decode a scope row, or the scope constrains only
		// dimensions this request does not describe. Either way the grant does
		// not speak to any key.
		return nil, nil, faultf(http.StatusForbidden, d.emptyCode,
			"the %s grant is scoped, but names no %s %s this daemon can act on; "+
				"the scope must carry %s (e.g. --scope %s)", perm, d.product, d.noun, d.dim, d.example)
	}
	for _, key := range named {
		if evalPermissionScope(v, convID, d.context(key)).Satisfied {
			grantKeys = appendScopeKey(grantKeys, key)
		}
	}
	if len(grantKeys) == 0 {
		// The scope names keys but admits none of them, so something else in the
		// same scope refused. The operator's list is not the thing to edit.
		return nil, nil, faultf(http.StatusForbidden, d.emptyCode,
			"the %s grant names %s(s) %s, but its scope also constrains a dimension %s "+
				"does not describe, so it authorizes nothing; the scope must constrain "+
				"%s alone", perm, d.noun, strings.Join(lowerScopeKeys(named), ", "), d.request, d.dim)
	}
	if len(d.allowed) == 0 {
		// No operator list: the scope is the whole policy.
		return grantKeys, grantKeys, nil
	}
	for _, key := range grantKeys {
		if d.allowedKey(key) {
			keys = appendScopeKey(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil, grantKeys, faultf(http.StatusForbidden, d.emptyCode,
			"the %s grant is scoped to %s(s) %s, none of which is on the operator's "+
				"%s list (allowed: %s); the two must overlap",
			perm, d.noun, strings.Join(grantKeys, ", "), d.listKey, strings.Join(d.allowed, ", "))
	}
	return keys, grantKeys, nil
}

// lowerScopeKeys normalizes a key list the way the operators' allow-lists are
// normalized, so the two are directly comparable and render alike.
func lowerScopeKeys(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = appendScopeKey(out, key)
	}
	return out
}

// appendScopeKey adds one normalized (trimmed, lower-cased) key to a list,
// dropping blanks and duplicates. Order is preserved: these lists are rendered
// to humans in refusals and in `whoami`, so they keep the order the operator
// wrote.
func appendScopeKey(out []string, key string) []string {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" || slices.Contains(out, key) {
		return out
	}
	return append(out, key)
}
