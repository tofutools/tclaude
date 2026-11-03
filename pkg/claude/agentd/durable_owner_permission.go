package agentd

import "net/http"

// durableOwnerAllows re-checks, outside any HTTP request, whether a stored
// owner still holds slug for actx. A stored owner is the owning agent of a
// schedule or trigger, which acts later, long after it was authorized. The
// check is the same one a live request gets from permissionAllowsAction.
// The one addition: for group actions, owning the group still counts
// (structuralPermissionMatch) unless the verdict is an explicit deny.
//
// The permission helpers take an *http.Request for request-scoped state
// (peer actions, approval continuations, the sudo-use meter). A durable owner
// has none, so a bare request stands in. This is the one place that builds it;
// source names the caller in its URL ("cron", "trigger").
//
// The error is permissionAllowsAction's own. The structural fallback is
// still evaluated when it is set, so a caller that chooses to ignore the
// error sees the same verdict it always did.
func durableOwnerAllows(source, conv, slug string, actx ActionContext) (bool, error) {
	req, _ := http.NewRequest(http.MethodPost, "http://"+source+".invalid", nil)
	allowed, _, authErr := permissionAllowsAction(req, conv, slug, actx)
	if !allowed && actx.structuralGroup != "" &&
		resolvePermissionVerdictForRequest(req, conv, slug).Resolution != permDeny {
		allowed, _ = structuralPermissionMatch(conv, slug, actx)
	}
	return allowed, authErr
}
