package agentd

import (
	"strings"
	"testing"
)

// TestDashboardJS_ExportModalWired guards the end-to-end wiring of the per-agent
// "📋 summary…" export across the embedded dashboard source: the ⚙-menu button,
// its row-action case, the modal element + its key controls, and the boot
// binding. A drop in any one silently breaks the feature at runtime.
func TestDashboardJS_ExportModalWired(t *testing.T) {
	for _, needle := range []string{
		`data-act="export-summary"`,      // ⚙-menu button (helpers.js)
		"function exportAgentButton(",    // its builder (helpers.js)
		"case 'export-summary'",          // row-action dispatch (row-actions.js)
		"openExportModal(agent, label)",  // dispatch opens the modal (row-actions.js)
		"function bindExportModal(",      // modal binder (modal-export.js)
		"bindExportModal();",             // boot wires it (dashboard.js)
		`id="export-agent-modal"`,        // modal element (dashboard.html)
		`id="export-agent-instructions"`, // instructions field (dashboard.html)
		"/api/export-jobs/",              // poll/download endpoint (modal-export.js)
		`id="export-agent-history"`,      // history panel (dashboard.html)
		"function loadExportHistory(",    // history loader (modal-export.js)
		"/exports",                       // list/clear endpoint (modal-export.js)
		`data-export-act="delete"`,       // per-entry delete control (modal-export.js)
		// The working-phase step checklist (export-progress.js).
		`id="export-agent-checklist"`,     // checklist mount (dashboard.html)
		"function renderExportChecklist(", // modal checklist renderer (export-progress.js)
		"function updateChecklist(",       // status-change repaint (modal-export.js)
		// The Jobs tab's unified job table (exports + cron).
		`data-tab="jobs"`,                       // the extended Cron→Jobs nav tab (dashboard.html)
		`id="jobs-root"`,                        // Preact feature mount (dashboard.html)
		"function JobsApp(",                     // island renderer (jobs-island.js)
		"get('/api/jobs?' + jobs.params.value)", // windowed unified fetch (refresh.js)
		"function ExportStepper(",               // in-flight Preact row stepper
		"actions.downloadExport(job)",           // component-owned download action
		"dismissExport: async (job)",            // component action boundary
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q — export modal wiring broken", needle)
		}
	}
}

func TestSanitizeExportFilename(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"summary.md", "summary.md"},
		{"", "export.zip"},
		{"   ", "export.zip"},
		{"../../etc/passwd", "passwd"},
		{"/abs/path/report.zip", "report.zip"},
		{`..\..\win.txt`, "win.txt"},
		{".", "export.zip"},
		{"..", "export.zip"},
		{"a/b/c.md", "c.md"},
		{"with\nnewline.md", "withnewline.md"},
		{`foo".zip`, "foo.zip"}, // quote would break Content-Disposition
		{"a;b.md", "ab.md"},     // semicolon too
	}
	for _, c := range cases {
		if got := sanitizeExportFilename(c.in); got != c.want {
			t.Errorf("sanitizeExportFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// Overlong names are truncated to a bounded length.
	long := make([]byte, 500)
	for i := range long {
		long[i] = 'a'
	}
	if got := sanitizeExportFilename(string(long)); len(got) > 200 {
		t.Errorf("expected truncation to <=200, got len %d", len(got))
	}
}
