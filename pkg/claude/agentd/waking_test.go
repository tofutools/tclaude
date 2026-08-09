package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

func TestMarkConvWakingBracketsTheAttempt(t *testing.T) {
	const conv = "ses_waking_test"
	if isConvWaking(conv) {
		t.Fatal("conversation must not start waking")
	}
	clear := markConvWaking(conv)
	if !isConvWaking(conv) {
		t.Fatal("conversation must report waking while a resume is in flight")
	}
	clear()
	if isConvWaking(conv) {
		t.Fatal("clear must remove the waking flag whatever the outcome")
	}
}

// The waking presentation is a cross-file contract: the snapshot field, the
// row renderer, the optimistic click-time surgery, and the CSS classes have
// to agree on names. Pin the load-bearing pieces the same way the resume
// error-toast contract is pinned.
func TestDashboardWakingPresentationContract(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		body, err := fs.ReadFile(dashboardAssetsFS, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(body)
	}
	table := read("js/groups-member-table.js")
	for _, required := range []string{
		"member.waking",
		"status-dot-waking",
		"state-waking",
	} {
		if !strings.Contains(table, required) {
			t.Errorf("member table is missing waking contract %q", required)
		}
	}
	operations := read("js/dashboard-operations.js")
	for _, required := range []string{
		"markRowsWaking(conv);",
		"clearRowsWaking(conv);",
	} {
		if !strings.Contains(operations, required) {
			t.Errorf("resume operation is missing waking contract %q", required)
		}
	}
	css := read("dashboard.css")
	for _, required := range []string{
		".status-dot-waking",
		"@keyframes dot-waking",
		"prefers-reduced-motion",
		".state-waking",
	} {
		if !strings.Contains(css, required) {
			t.Errorf("dashboard.css is missing waking contract %q", required)
		}
	}
}
