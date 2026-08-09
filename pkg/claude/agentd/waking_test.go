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

// Two resumes of the same conversation can overlap (group bulk resume racing
// a dashboard click). The first to finish must not unflag a wake still in
// flight, and a double-cleared bracket must not eat a later mark.
func TestMarkConvWakingOverlappingAttempts(t *testing.T) {
	const conv = "ses_waking_overlap"
	first := markConvWaking(conv)
	second := markConvWaking(conv)
	first()
	if !isConvWaking(conv) {
		t.Fatal("the second in-flight resume must keep the conversation waking")
	}
	first() // double clear of the same bracket is a no-op, not a decrement
	if !isConvWaking(conv) {
		t.Fatal("re-running one bracket's clear must not unflag another's")
	}
	second()
	if isConvWaking(conv) {
		t.Fatal("last clear must remove the flag")
	}
}

// The waking presentation is a cross-file contract: the snapshot field, the
// row renderer, the optimistic pending-wake state, and the CSS classes have
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
		"isPendingWake",
		"clearPendingWake",
		"status-dot-waking",
		"state-waking",
	} {
		if !strings.Contains(table, required) {
			t.Errorf("member table is missing waking contract %q", required)
		}
	}
	operations := read("js/dashboard-operations.js")
	for _, required := range []string{
		"markPendingWake(conv);",
		"clearPendingWake(conv);",
	} {
		if !strings.Contains(operations, required) {
			t.Errorf("resume operation is missing waking contract %q", required)
		}
	}
	pendingState := read("js/waking-state.js")
	for _, required := range []string{
		"PENDING_WAKE_TTL_MS",
		"export function markPendingWake",
		"export function clearPendingWake",
		"export function isPendingWake",
	} {
		if !strings.Contains(pendingState, required) {
			t.Errorf("waking-state module is missing contract %q", required)
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
