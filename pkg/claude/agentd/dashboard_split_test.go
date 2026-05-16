package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// The dashboard UI used to be one file, dashboard.html, with the CSS
// and JS inline. They were extracted into sibling files (dashboard.css,
// dashboard.js) for editor tooling, and assembleDashboardHTML() splices
// them back into the markup shell at package init. The extraction is a
// purely mechanical relocation — the bytes served to the browser must
// stay IDENTICAL to the pre-split single-file dashboard.html.
//
// preSplitDashboardSHA256 is the SHA-256 of dashboard.html as it stood
// at commit 31dfeaa, immediately before the split. Reproduce it:
//
//	git show 31dfeaa:pkg/claude/agentd/dashboard.html | sha256sum
//
// A future INTENTIONAL change to the dashboard UI updates this constant
// deliberately (recompute it from the assembled bytes) — that small
// friction is the point: it keeps any change to the served bytes a
// conscious, reviewed act rather than an accident.
const preSplitDashboardSHA256 = "6ec67179269bd47a3cd860f273efec6fd66313c062a74bf12ffef062a36f1917"

// TestDashboardHTML_SplitIsByteIdentical is the load-bearing guard for
// the CSS/JS extraction: it pins that assembleDashboardHTML() produces
// exactly the bytes the one-file dashboard.html used to hold.
func TestDashboardHTML_SplitIsByteIdentical(t *testing.T) {
	sum := sha256.Sum256([]byte(dashboardHTML))
	got := hex.EncodeToString(sum[:])
	if got != preSplitDashboardSHA256 {
		t.Fatalf("assembled dashboardHTML SHA-256 = %s\n"+
			"                            want = %s\n"+
			"the CSS/JS extraction is not byte-identical to the pre-split "+
			"dashboard.html — the split must be a pure mechanical relocation. "+
			"If you changed the dashboard UI on purpose, recompute the "+
			"constant from the new assembled bytes.", got, preSplitDashboardSHA256)
	}
}

// TestDashboardHTML_AssemblySpliceIsClean is content-agnostic: it
// checks the splice fired correctly and left a well-formed single
// document — both placeholders consumed, both halves present in full,
// one <style> / <script> element each. Unlike the SHA-256 pin above it
// survives future intentional content edits, so it stays a permanent
// guard on the reassembly logic. Byte-identity itself is proven only
// by TestDashboardHTML_SplitIsByteIdentical; this is its debuggable
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
