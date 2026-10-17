package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_MessageFilterAboveList guards the Messages-tab layout
// where the message filter (#filter-messages) was moved out of a separate
// top filter bar and made a column header that sits right above the
// message list — mirroring the recipient/mailbox filter (#filter-mailboxes)
// above the sidebar. The change spans dashboard.html (DOM order + the
// .mail-list-filter wrapper) and dashboard.css (the 2-row grid placement),
// and the two filter <input>s must stay DOM-adjacent so Tab moves
// recipient-filter → message-filter directly. The repo has no JS/DOM test
// runner, so this asserts on the embedded source at `go test ./...`.
func TestDashboardHTML_MessageFilterAboveList(t *testing.T) {
	island := string(mustReadFS(dashboardAssetsFS, "js/mail-island.js"))
	// The filters are a shared Messages part (mail-parts.js); the island
	// places them in its mail client and names this node's IDs.
	html := string(mustReadFS(dashboardAssetsFS, "js/mail-parts.js"))

	// Both filters still exist, and the message filter lives inside the mail
	// client (its grid), not a standalone top .filter-bar.
	mailClient := strings.Index(island, `class="mail-client"`)
	if mailClient < 0 {
		t.Fatal("mail-island.js: .mail-client container not found")
	}
	use := strings.Index(island, "<${MailFilters}")
	if use < mailClient {
		t.Error("mail-island.js: the filters should sit inside .mail-client (above the list), not in a top filter bar")
	}
	for _, needle := range []string{`boxId="filter-mailboxes"`, `id="filter-messages"`, `clearId="filter-messages-clear"`} {
		if !strings.Contains(island[use:], needle) {
			t.Errorf("mail-island.js: the Messages-tab filters are missing %q", needle)
		}
	}
	boxIdx := strings.Index(html, `<input id=${boxId}`)
	msgIdx := strings.Index(html, `<input id=${id}`)
	if boxIdx < 0 || msgIdx < 0 {
		t.Fatal("mail-parts.js: a Messages-tab filter input is missing")
	}

	// Tab order = DOM order: the message filter must come AFTER the
	// recipient filter (it used to come before), and DIRECTLY after — no
	// other focusable element may sit between the two inputs. Measure the
	// gap strictly between the two <input> elements: from the end of the
	// recipient input's tag to the start of the message input's tag (so
	// neither input's own markup counts as "in between").
	if msgIdx <= boxIdx {
		t.Error("mail-parts.js: the message filter must come after the mailbox filter in DOM (so Tab reaches the recipient filter first)")
	}
	boxTagEnd := strings.Index(html[boxIdx:], "/>")
	msgTagStart := msgIdx // the needle starts at the message input's tag
	if boxTagEnd < 0 {
		t.Fatal("mail-parts.js: could not bound the gap between the two filter inputs")
	}
	between := html[boxIdx+boxTagEnd : msgTagStart]
	for _, focusable := range []string{"<input", "<button", "<select", "<textarea", "<a ", "tabindex"} {
		if strings.Contains(between, focusable) {
			t.Errorf("mail-parts.js: a focusable %q sits between the mailbox and message filters — they must be Tab-adjacent", focusable)
		}
	}

	// The message filter is wrapped in .mail-list-filter with only its count
	// and clear button; the folder-wide actions moved into the bulk bar
	// (.mail-folder-actions in MessageBulkBar) so they never squeeze the
	// filter input.
	if !strings.Contains(html, `<div class="mail-list-filter">`) {
		t.Error("mail-parts.js missing the .mail-list-filter row")
	}
	for _, needle := range []string{
		`'mail-folder-actions'`,
		`id="mail-mark-all"`,
		`id="mail-clear-read"`,
		`>🗑 delete read</button>`,
	} {
		if !strings.Contains(island, needle) {
			t.Errorf("mail-island.js missing %q in the bulk-bar row", needle)
		}
	}
	filterStart := strings.Index(html, `<div class="mail-list-filter">`)
	if filterStart < 0 {
		t.Fatal("mail-parts.js: .mail-list-filter row not found")
	}
	filterRow := html[filterStart:]
	filterRow = filterRow[:strings.Index(filterRow, "</div>")]
	for _, id := range []string{`id="mail-mark-all"`, `id="mail-peer-compose"`, `id="mail-clear-read"`, `id="mail-agent-mark-all"`} {
		if strings.Contains(filterRow, id) {
			t.Errorf("mail-parts.js: %s must sit in the bulk bar, not the filter row", id)
		}
	}

	// CSS: the mail client is a 2-row grid and each child is placed into its
	// cell — the message filter on row 1, the bodies on row 2. The column
	// tracks are sidebar(1) | gutter(2) | list(3) | gutter(4) | reader(5)
	// after the draggable-column work (#458) inserted the two 10px gutter
	// tracks, so the list/filter sit in column 3 and the reader in column 5.
	for _, needle := range []string{
		"grid-template-rows: auto 1fr;",
		".mail-list-filter { grid-column: 3; grid-row: 1; }",
		".mail-list-col    { grid-column: 3; grid-row: 2; }",
		".mail-reader      { grid-column: 5; grid-row: 1 / span 2; }",
		".mail-list-filter {",    // the relocated row is styled
		".mail-folder-actions {", // the folder-wide actions in the bulk bar are styled
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard.css missing %q — Messages-tab grid layout regressed", needle)
		}
	}
}

// TestDashboardHTML_DeleteReadMessagesConfirms guards the destructive human-
// notification sweep: its label says what it does, and the shared confirmation
// dialog remains between the click and the delete request.
func TestDashboardHTML_DeleteReadMessagesConfirms(t *testing.T) {
	js := string(mustReadFS(dashboardAssetsFS, "js/row-action-handler.js"))
	caseStart := strings.Index(js, `case 'msg-clear':`)
	if caseStart < 0 {
		t.Fatal("row-action-handler.js: msg-clear case missing")
	}
	caseEnd := strings.Index(js[caseStart:], `case 'msg-delete':`)
	if caseEnd < 0 {
		t.Fatal("row-action-handler.js: could not bound msg-clear case")
	}
	clearCase := js[caseStart : caseStart+caseEnd]

	for _, needle := range []string{
		`case 'msg-clear':`,
		`title: 'Delete read messages?'`,
		`body: 'Permanently deletes every message that has been marked read. Unread messages are kept.'`,
		`okLabel: 'Delete read'`,
		`if (!confirmed) return;`,
		`fetch('/api/human-messages/clear'`,
	} {
		if !strings.Contains(clearCase, needle) {
			t.Errorf("row-action-handler.js missing %q — delete-read confirmation regressed", needle)
		}
	}

	confirmIdx := strings.Index(clearCase, `title: 'Delete read messages?'`)
	cancelIdx := strings.Index(clearCase, `if (!confirmed) return;`)
	deleteIdx := strings.Index(clearCase, `fetch('/api/human-messages/clear'`)
	if confirmIdx < 0 || cancelIdx < 0 || deleteIdx < 0 || confirmIdx >= cancelIdx || cancelIdx >= deleteIdx {
		t.Error("delete-read action must confirm, honor cancellation, then make the destructive request")
	}
}
