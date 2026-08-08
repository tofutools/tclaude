package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// A bulk shutdown / power-on must show the daemon's PER-AGENT reason for every
// agent it could not handle, not just the aggregate count.
//
// The behaviour itself is unit-tested in jstest/bulk-power-failures.test.mjs,
// but that test drives the helper directly — deleting the call sites leaves it,
// and every other suite, green while the dashboard silently reverts to
// reporting "1 failed" and nothing else. That was the original defect: the
// daemon knew the resume had died with "duplicate session: …", said so in its
// response, and the only place it survived was the log. This pins the wiring.
func TestDashboardBulkPowerFailuresAreReported(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		data, err := fs.ReadFile(dashboardAssetsFS, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data)
	}
	operations := read("js/dashboard-operations.js")
	report := read("js/bulk-power-report.js")
	css := read("dashboard.css")

	for _, required := range []string{
		"import { reportBulkPowerFailures } from './bulk-power-report.js';",
		"await reportBulkPowerFailures('Shutdown', out);",
		"await reportBulkPowerFailures('Power on', out);",
	} {
		if !strings.Contains(operations, required) {
			t.Errorf("dashboard-operations.js no longer reports bulk power failures: missing %q", required)
		}
	}

	// The filter is "not a success" rather than "== failed" so it agrees with
	// the daemon's own bucketing, which folds anything unrecognized into
	// Failed. Matching the failure name instead would let a renamed outcome
	// make the toast say "1 failed" while the modal listed nothing.
	for _, required := range []string{
		"POWER_SUCCESS_OUTCOMES", "'exited_gracefully'", "'force_killed'",
		"'already_offline'", "'resumed'", "'already_online'",
		"!POWER_SUCCESS_OUTCOMES.has(",
	} {
		if !strings.Contains(report, required) {
			t.Errorf("bulk-power-report.js no longer excludes successes by name: missing %q", required)
		}
	}

	// .modal has no height bound and .modal-overlay centres without scrolling,
	// so an unbounded failure list pushes its own Close button off-screen —
	// the content this modal exists to show becomes unreachable.
	if !strings.Contains(report, "MAX_LISTED_FAILURES") {
		t.Error("bulk-power-report.js no longer caps the listed failures")
	}
	for _, required := range []string{"max-height: 50vh", "overflow-y: auto"} {
		if !strings.Contains(css, required) {
			t.Errorf("the preformatted confirm body is no longer bounded: missing %q", required)
		}
	}
}
