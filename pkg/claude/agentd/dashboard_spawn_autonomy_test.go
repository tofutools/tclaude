package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func spawnEffectiveSandbox(t *testing.T, query string) (int, spawnEffectiveSandboxJSON) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handleDashboardSpawnEffectiveSandbox(recorder, dashboardRequest(http.MethodGet, "/api/spawn/effective-sandbox?"+query, ""))
	var payload spawnEffectiveSandboxJSON
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode body %q: %v", recorder.Body.String(), err)
		}
	}
	return recorder.Code, payload
}

// The dashboard probe must answer for the posture the spawn would ACTUALLY get.
// A dialog whose selects are on their blank "default" option sends empty
// values, and answering those as "nothing chosen, nothing to warn about" would
// stay silent on exactly the default Claude spawn TCL-586 is about.
func TestDashboardSpawnEffectiveSandboxAppliesHarnessDefaults(t *testing.T) {
	withDashboardAuth(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	status, payload := spawnEffectiveSandbox(t, "harness=claude&sandbox=&approval=&dir="+home)
	if status != http.StatusOK {
		t.Fatalf("got status %d, want 200", status)
	}
	if payload.Approval != "auto" {
		t.Fatalf("got approval %q, want the resolved harness default %q", payload.Approval, "auto")
	}
	if payload.SandboxState != "unconfigured" {
		t.Fatalf("got sandbox_state %q, want unconfigured", payload.SandboxState)
	}
	if len(payload.Warnings) == 0 || !strings.Contains(payload.Warnings[0], "unattended") {
		t.Fatalf("got warnings %v, want the unsandboxed-autonomy warning", payload.Warnings)
	}
}

func TestDashboardSpawnEffectiveSandboxSilentWhenSandboxed(t *testing.T) {
	withDashboardAuth(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"),
		[]byte(`{"sandbox":{"enabled":true}}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	status, payload := spawnEffectiveSandbox(t, "harness=claude&approval=auto&dir="+home)
	if status != http.StatusOK {
		t.Fatalf("got status %d, want 200", status)
	}
	if payload.SandboxState != "on" {
		t.Fatalf("got sandbox_state %q, want on", payload.SandboxState)
	}
	if len(payload.Warnings) != 0 {
		t.Fatalf("got warnings %v, want none", payload.Warnings)
	}
	// An always-present array keeps every consumer free of a null guard.
	if !strings.Contains(httpBodyOf(t, "harness=claude&approval=auto&dir="+home), `"warnings":[]`) {
		t.Fatal("empty warnings should serialize as [], not null")
	}
}

// The same probe that drives the Claude TCL-586 warning must also surface
// OpenCode's "access-control is not a real sandbox" line, so the spawn dialog
// and the profile/role editors warn when an OpenCode agent's sandbox is on.
func TestDashboardSpawnEffectiveSandboxOpenCodeAccessControl(t *testing.T) {
	withDashboardAuth(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A blank sandbox select resolves to OpenCode's access-control default —
	// the exact posture a user gets without choosing one — and must warn.
	status, payload := spawnEffectiveSandbox(t, "harness=opencode&sandbox=&approval=&dir="+home)
	if status != http.StatusOK {
		t.Fatalf("got status %d, want 200", status)
	}
	if payload.SandboxMode != "access-control" {
		t.Fatalf("got sandbox_mode %q, want the resolved access-control default", payload.SandboxMode)
	}
	if len(payload.Warnings) == 0 || !strings.Contains(payload.Warnings[0], "no built-in OS sandbox") {
		t.Fatalf("got warnings %v, want the OpenCode sandbox warning", payload.Warnings)
	}
	// SandboxState/SandboxSource describe Claude's settings.json sandbox; they
	// must not be resolved (and possibly report "on") for an OpenCode spawn,
	// which would contradict the warning above. Set a Claude sandbox in the same
	// HOME to prove the OpenCode path ignores it.
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"),
		[]byte(`{"sandbox":{"enabled":true}}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	_, again := spawnEffectiveSandbox(t, "harness=opencode&sandbox=&approval=&dir="+home)
	if again.SandboxState != "unconfigured" || again.SandboxSource != "" {
		t.Fatalf("opencode echoed Claude sandbox state %q/%q, want unconfigured/\"\"",
			again.SandboxState, again.SandboxSource)
	}

	// Explicitly turning scoping off is a deliberate opt-out with its own ⚠ in
	// the mode help, so the probe stays silent there.
	if _, off := spawnEffectiveSandbox(t, "harness=opencode&sandbox=off&approval=&dir="+home); len(off.Warnings) != 0 {
		t.Fatalf("opencode off: got warnings %v, want none", off.Warnings)
	}
}

func httpBodyOf(t *testing.T, query string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	handleDashboardSpawnEffectiveSandbox(recorder, dashboardRequest(http.MethodGet, "/api/spawn/effective-sandbox?"+query, ""))
	return recorder.Body.String()
}

// A `~/…` CWD that reached the resolver unexpanded would find no project
// settings and report a clean bill of health for a directory it never looked
// at — the one failure mode this endpoint must not have.
func TestDashboardSpawnEffectiveSandboxExpandsTildeCwd(t *testing.T) {
	withDashboardAuth(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "repo")
	if err := os.MkdirAll(filepath.Join(project, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(project, ".claude", "settings.json"),
		[]byte(`{"sandbox":{"enabled":true}}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	_, payload := spawnEffectiveSandbox(t, "harness=claude&approval=auto&dir=~/repo")
	if payload.SandboxState != "on" {
		t.Fatalf("got sandbox_state %q, want on (tilde CWD should have been expanded)", payload.SandboxState)
	}
}

func TestDashboardSpawnEffectiveSandboxRejectsBadInput(t *testing.T) {
	withDashboardAuth(t)
	for _, tc := range []struct{ name, query string }{
		{"unknown harness", "harness=nope"},
		{"invalid sandbox", "harness=claude&sandbox=sideways"},
		{"invalid approval", "harness=claude&approval=whenever"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status, _ := spawnEffectiveSandbox(t, tc.query); status != http.StatusBadRequest {
				t.Fatalf("got status %d, want 400", status)
			}
		})
	}
}

// The endpoint drives filesystem reads off a caller-supplied dir and reports a
// security-relevant verdict, so it must reject a request without the dashboard
// cookie — like every one of its neighbours on the popup mux. A cold review
// caught the gate missing; this pins it. Calling with a plain (uncookied)
// request rather than dashboardRequest is the whole point.
func TestDashboardSpawnEffectiveSandboxRequiresAuth(t *testing.T) {
	withDashboardAuth(t)
	recorder := httptest.NewRecorder()
	handleDashboardSpawnEffectiveSandbox(recorder,
		httptest.NewRequest(http.MethodGet, "/api/spawn/effective-sandbox?harness=claude", nil))
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("got status %d, want 403 for an uncookied request", recorder.Code)
	}
}

// The handler tests above call it directly, which would keep passing if the
// route were never wired. The dialog only works if the path is actually served.
func TestDashboardSpawnEffectiveSandboxRouteIsRegistered(t *testing.T) {
	mux := http.NewServeMux()
	registerDashboardEditRoutes(mux)
	_, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, "/api/spawn/effective-sandbox?harness=claude", nil))
	if pattern != "/api/spawn/effective-sandbox" {
		t.Fatalf("got pattern %q, want the effective-sandbox route", pattern)
	}
}

func TestDashboardSpawnEffectiveSandboxRejectsNonGET(t *testing.T) {
	withDashboardAuth(t)
	recorder := httptest.NewRecorder()
	handleDashboardSpawnEffectiveSandbox(recorder,
		dashboardRequest(http.MethodPost, "/api/spawn/effective-sandbox", ""))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got status %d, want 405", recorder.Code)
	}
}
