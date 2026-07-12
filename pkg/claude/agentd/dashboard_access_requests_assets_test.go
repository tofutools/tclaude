package agentd

import (
	"strings"
	"testing"
)

// The access-requests folder is a client-side surface driven by the snapshot's
// access_requests[] — these needles pin the load-bearing wiring so a JS/HTML/CSS
// refactor that silently drops a piece fails here (the frontend is otherwise
// unexercised by Go tests). Grouped by concern for a legible failure.
func TestDashboardAssets_AccessRequestsWired(t *testing.T) {
	island := string(mustReadFS(dashboardAssetsFS, "js/mail-island.js"))
	for _, needle := range []string{
		// Folder identity + the snapshot-fed synthetic sidebar entry.
		"const ACCESS_ID = 'access-requests';",
		"function accessRequestsMailbox(",
		"lastSnapshot.access_requests_pending",
		"lastSnapshot.access_requests.length",
		"return { id: ACCESS_ID, kind: 'access-requests', unread: pending, total",
		// The decision endpoint call + the four decisions.
		"/api/access-requests/${encodeURIComponent(id)}/decision",
		"controller.decideAccess(request.id, 'approve')",
		"controller.decideAccess(request.id, 'deny')",
		"controller.decideAccess(request.id, 'always')",
		"controller.decideAccess(request.id, 'extend')",
		// Preact owns keyed request rows and dispatches selection directly.
		"controller.selectMessage(request.id)",
		"model.pendingAccess.map(request =>",
		"model.handledAccess.map(request =>",
		"class=${`mail-row access-row-item${active ? ' active' : ''}${handled ? '' : ' unread'}${attention ? ' access-attn' : ''}`}",
		// The selected request renders in the reader pane, matching the
		// human/all mail split between list and detail.
		"function accessRequestById(",
		"const request = model.allAccess.find(",
		`<div class="mail-reader-body access-reader-body">`,
		// The attention affordances: blinking nav badge + non-blocking banner.
		"badge.classList.toggle('blink', accessPending > 0)",
		`id="access-banner"`,
		`id="access-banner-review"`,
		// Deep link (?tab=messages&access_request=<id>) + the tick wiring.
		"function focusAccessRequest(",
		"dlParams.get('access_request')",
		"renderAccessRequests(data.access_requests || [], data.access_requests_pending || 0)",
		// Recently-handled history: outcome chips + the divider stay in the list.
		"function accessOutcome(",
		"function accessIsPending(",
		`key="__access_handled__"`,
		// CSS presence (row + reader actions + blink + handled history).
		".access-row-wrap.handled .mail-row",
		".access-reader-actions",
		".tab-badge.blink {",
		".access-outcome {",
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard assets missing %q — access-requests UI wiring broken", needle)
		}
	}

	// Preact text interpolation escapes the agent-supplied body. Keep it as a
	// child value and never opt this surface into raw HTML rendering.
	if !strings.Contains(island, `<pre class="access-body">${request.body}</pre>`) {
		t.Error("access-request body preview must remain a Preact text child (XSS gate)")
	}
	if strings.Contains(island, "dangerouslySetInnerHTML") {
		t.Error("access-request island must not render agent-supplied raw HTML")
	}
}
