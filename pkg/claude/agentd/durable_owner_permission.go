package agentd

import "net/http"

// durableOwnerAllows re-checks, outside any HTTP request, whether a stored
// owner still holds slug for actx. A stored owner is the owning agent of a
// schedule or trigger, which acts later, long after it was authorized. The
// check is the one a live request gets from permissionAllowsAction, which
// already counts group ownership and membership (structuralPermissionMatch)
// for any verdict short of an explicit deny.
//
// The permission helpers take an *http.Request for request-scoped state
// (peer actions, approval continuations, the sudo-use meter). A durable owner
// has none, so a bare request stands in. This is the one place that builds it;
// source names the caller in its URL ("cron", "trigger").
func durableOwnerAllows(source, conv, slug string, actx ActionContext) (bool, error) {
	req, _ := http.NewRequest(http.MethodPost, "http://"+source+".invalid", nil)
	allowed, _, err := permissionAllowsAction(req, conv, slug, actx)
	return allowed, err
}
