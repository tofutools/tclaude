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
		`data-act="export-summary"`,             // ⚙-menu button (helpers.js)
		"function exportAgentButton(",           // its builder (helpers.js)
		"case 'export-summary'",                 // row-action dispatch (row-actions.js)
		"openExportModal(conv, label)",          // dispatch opens the modal (row-actions.js)
		"function bindExportModal(",             // modal binder (modal-export.js)
		"bindExportModal();",                    // boot wires it (dashboard.js)
		`id="export-agent-modal"`,               // modal element (dashboard.html)
		`id="export-agent-instructions"`,        // instructions field (dashboard.html)
		"/api/export-jobs/",                     // poll/download endpoint (modal-export.js)
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
