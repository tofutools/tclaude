package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// Resume lifecycle results use an HTTP-200 envelope even when the per-agent
// action failed. The dashboard must not paint error:* results as ordinary
// success feedback or report them as a successful wake to its callers.
func TestDashboardResumeLifecycleErrorsUseErrorToast(t *testing.T) {
	body, err := fs.ReadFile(dashboardAssetsFS, "js/dashboard-operations.js")
	if err != nil {
		t.Fatalf("read dashboard operations: %v", err)
	}
	source := string(body)

	recovery := "if (out.action === 'error:missing_cwd') {"
	generic := "if (action === 'error' || action.startsWith('error:')) {"
	for _, required := range []string{
		"const action = String(out.action || '');",
		generic,
	} {
		if !strings.Contains(source, required) {
			t.Errorf("dashboard resume is missing lifecycle-error contract %q", required)
		}
	}
	recoveryAt, genericAt := strings.Index(source, recovery), strings.Index(source, generic)
	if recoveryAt < 0 || genericAt < 0 || recoveryAt > genericAt {
		t.Error("missing-cwd recovery must run before generic error:* toast handling")
		return
	}
	successAt := strings.Index(source[genericAt:], "toast(`wake ${label}: ${action || 'ok'}`);")
	if successAt < 0 {
		t.Fatal("dashboard resume is missing its success-toast boundary")
	}
	errorBranch := source[genericAt : genericAt+successAt]
	for _, required := range []string{
		"const detail = out.detail ? ` — ${out.detail}` : '';",
		"toast(`wake ${label}: ${action}${detail}`, true);",
		"return false;",
	} {
		if !strings.Contains(errorBranch, required) {
			t.Errorf("generic lifecycle-error branch is missing %q", required)
		}
	}
	if strings.Contains(errorBranch, "refresh()") {
		t.Error("generic lifecycle-error branch must not refresh as though resume succeeded")
	}
}
