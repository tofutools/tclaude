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
	for _, needle := range []string{
		// Folder identity + the snapshot-fed synthetic sidebar entry.
		"const ACCESS_ID = 'access-requests';",
		"function accessRequestsMailbox(",
		"lastSnapshot.access_requests_pending",
		"lastSnapshot.access_requests.length",
		"return { id: ACCESS_ID, kind: 'access-requests', unread: pending, total",
		// The decision endpoint call + the four decisions.
		"/api/access-requests/${encodeURIComponent(id)}/decision",
		`data-act="access-open"`,
		`data-act="access-approve"`,
		`data-act="access-deny"`,
		`data-act="access-always"`,
		`data-act="access-extend"`,
		// Dispatch → decideAccess wiring.
		"selectMessage(btn.getAttribute('data-id'))",
		"decideAccess(btn.getAttribute('data-id'), 'approve')",
		"decideAccess(btn.getAttribute('data-id'), 'deny')",
		// Rows render through the keyed reconciler (not innerHTML), so
		// selection / the deep-link highlight survives the 2s repaint.
		"pending.map(accessRowHTML).join('')",
		"handled.map(accessRowHTML).join('')",
		"morphInto(el, html)",
		`class="mail-row access-row-item${active ? ' active' : ''}${!handled ? ' unread' : ''}${attn}"`,
		// The selected request renders in the reader pane, matching the
		// human/all mail split between list and detail.
		"function accessRequestById(",
		"function accessActionsHTML(",
		"const r = accessRequestById(mail.selectedMsgId)",
		"<div class=\"mail-reader-body access-reader-body\">",
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
		`data-key="__access_handled__"`,
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

	// The untrusted body preview MUST go through esc() before it lands in a
	// card — it's agent-supplied output, the injection gate the old
	// server-rendered popup used html.EscapeString for.
	if !strings.Contains(dashboardAssets, "<pre class=\"access-body\">${esc(r.body)}</pre>") {
		t.Error("access-request body preview must be esc()'d (XSS gate)")
	}
}
