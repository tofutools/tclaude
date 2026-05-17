package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// The dashboard UI is assembled from three sibling files —
// dashboard.html (the markup shell), dashboard.css and dashboard.js —
// which assembleDashboardHTML() splices together at package init.
//
// dashboardHTMLSHA256 pins the SHA-256 of those assembled bytes. It is
// a deliberate-change tripwire: any edit to the served dashboard moves
// the hash and fails the test below, so updating this constant is a
// conscious, reviewed act rather than an accident. Recompute it from
// the new assembled bytes whenever you change the dashboard UI on
// purpose — the failure message prints the new value to paste in.
const dashboardHTMLSHA256 = "b398e66c9afa1f68380a2ab970e222c132cc29faae76dc9d913827d47bf1b2d7"

// TestDashboardHTML_ServedBytesPinned guards the bytes
// assembleDashboardHTML() serves to the browser: it fails on any
// unreviewed change to the dashboard UI. An intentional change updates
// dashboardHTMLSHA256.
func TestDashboardHTML_ServedBytesPinned(t *testing.T) {
	sum := sha256.Sum256([]byte(dashboardHTML))
	got := hex.EncodeToString(sum[:])
	if got != dashboardHTMLSHA256 {
		t.Fatalf("assembled dashboardHTML SHA-256 = %s\n"+
			"                            want = %s\n"+
			"the served dashboard bytes changed. If you changed the "+
			"dashboard UI on purpose, recompute dashboardHTMLSHA256 from "+
			"the new assembled bytes (the value above).", got, dashboardHTMLSHA256)
	}
}

// TestDashboardHTML_AssemblySpliceIsClean is content-agnostic: it
// checks the splice fired correctly and left a well-formed single
// document — both placeholders consumed, both halves present in full,
// one <style> / <script> element each. Unlike the SHA-256 pin above it
// survives future intentional content edits, so it stays a permanent
// guard on the reassembly logic. The exact served bytes are pinned by
// TestDashboardHTML_ServedBytesPinned; this is its debuggable
// companion — it says *where* a broken splice went wrong.
func TestDashboardHTML_AssemblySpliceIsClean(t *testing.T) {
	// The shell carries exactly one empty <style></style> and one empty
	// <script></script> placeholder — the splice points.
	if n := strings.Count(dashboardShellHTML, "<style></style>"); n != 1 {
		t.Errorf("shell must hold exactly one empty <style></style> splice point, got %d", n)
	}
	if n := strings.Count(dashboardShellHTML, "<script></script>"); n != 1 {
		t.Errorf("shell must hold exactly one empty <script></script> splice point, got %d", n)
	}

	// The assembled page must have filled both — no empty placeholder
	// may survive.
	if strings.Contains(dashboardHTML, "<style></style>") {
		t.Error("assembled page still has an empty <style></style> — CSS was not spliced in")
	}
	if strings.Contains(dashboardHTML, "<script></script>") {
		t.Error("assembled page still has an empty <script></script> — JS was not spliced in")
	}

	// The splice actually fired and added exactly the CSS + JS bytes: a
	// Replace that matched nothing would leave dashboardHTML at
	// len(shell). (A sanity check on the splice, not a byte-identity
	// proof — that is TestDashboardHTML_SplitIsByteIdentical's job.)
	if got, want := len(dashboardHTML), len(dashboardShellHTML)+len(dashboardCSS)+len(dashboardJS); got != want {
		t.Errorf("assembled length %d != shell+css+js %d — the splice did not fire as expected", got, want)
	}

	// Both extracted halves must be present, in full, in the page.
	if dashboardCSS == "" || !strings.Contains(dashboardHTML, dashboardCSS) {
		t.Error("assembled page is missing the extracted CSS")
	}
	if dashboardJS == "" || !strings.Contains(dashboardHTML, dashboardJS) {
		t.Error("assembled page is missing the extracted JS")
	}

	// Still one well-formed document: a single <style> and <script>
	// element, each with its matching close tag.
	for _, tag := range []string{"<style>", "</style>", "<script>", "</script>"} {
		if n := strings.Count(dashboardHTML, tag); n != 1 {
			t.Errorf("assembled page must contain exactly one %q, got %d", tag, n)
		}
	}
}
