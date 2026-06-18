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
	html := string(dashboardIndexHTML)

	// Both filters still exist, and the message filter now lives inside the
	// mail client (its grid), not a standalone top .filter-bar.
	mailClient := strings.Index(html, `class="mail-client"`)
	if mailClient < 0 {
		t.Fatal("dashboard.html: .mail-client container not found")
	}
	boxIdx := strings.Index(html, `id="filter-mailboxes"`)
	msgIdx := strings.Index(html, `id="filter-messages"`)
	if boxIdx < 0 || msgIdx < 0 {
		t.Fatal("dashboard.html: a Messages-tab filter input is missing")
	}
	if msgIdx < mailClient {
		t.Error("dashboard.html: #filter-messages should sit inside .mail-client (above the list), not in a top filter bar")
	}

	// Tab order = DOM order: the message filter must come AFTER the
	// recipient filter (it used to come before), and DIRECTLY after — no
	// other focusable element may sit between the two inputs. Measure the
	// gap strictly between the two <input> elements: from the end of the
	// recipient input's tag to the start of the message input's tag (so
	// neither input's own markup counts as "in between").
	if msgIdx <= boxIdx {
		t.Error("dashboard.html: #filter-messages must come after #filter-mailboxes in DOM (so Tab reaches the recipient filter first)")
	}
	boxTagEnd := strings.IndexByte(html[boxIdx:], '>')
	msgTagStart := strings.LastIndex(html[:msgIdx], "<input")
	if boxTagEnd < 0 || msgTagStart < 0 {
		t.Fatal("dashboard.html: could not bound the gap between the two filter inputs")
	}
	between := html[boxIdx+boxTagEnd : msgTagStart]
	for _, focusable := range []string{"<input", "<button", "<select", "<textarea", "<a ", "tabindex"} {
		if strings.Contains(between, focusable) {
			t.Errorf("dashboard.html: a focusable %q sits between #filter-mailboxes and #filter-messages — they must be Tab-adjacent", focusable)
		}
	}

	// The message filter is wrapped in .mail-list-filter, which also hosts
	// the human-folder bulk actions relocated from the old top bar.
	for _, needle := range []string{
		`<div class="mail-list-filter">`,
		`id="mail-mark-all"`,
		`id="mail-clear-read"`,
		`id="filter-messages-clear"`, // the clear button bindFilter('messages') wires
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("dashboard.html missing %q in the relocated message-filter row", needle)
		}
	}

	// CSS: the mail client is a 2-row grid and each child is placed into its
	// cell — the message filter on row 1 (column 2), the bodies on row 2.
	for _, needle := range []string{
		"grid-template-rows: auto 1fr;",
		".mail-list-filter { grid-column: 2; grid-row: 1; }",
		".mail-list-col    { grid-column: 2; grid-row: 2; }",
		".mail-reader      { grid-column: 3; grid-row: 1 / span 2; }",
		".mail-list-filter {", // the relocated row is styled
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard.css missing %q — Messages-tab grid layout regressed", needle)
		}
	}
}
