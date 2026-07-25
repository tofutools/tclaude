package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDashboardInstanceEndpoint pins the restart marker a disconnected browser
// terminal polls while it waits to find out whether its socket died because
// agentd restarted (reattach) or for any of the ordinary reasons (don't).
//
// The two properties that matter: the id is CONSTANT for the life of one
// process — otherwise every probe would look like a restart and terminals
// would fight over attaches — and the endpoint requires dashboard auth like
// every other API surface.
func TestDashboardInstanceEndpoint(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	get := func() *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		handleDashboardInstance(rec, dashboardRequest(http.MethodGet, "/api/instance", ""))
		return rec
	}

	rec := get()
	if rec.Code != http.StatusOK {
		t.Fatalf("instance status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var first struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode instance: %v", err)
	}
	if first.InstanceID == "" {
		t.Fatal("instance_id is empty — a terminal can never tell a restart from any other disconnect")
	}

	var second struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal(get().Body.Bytes(), &second); err != nil {
		t.Fatalf("decode second instance: %v", err)
	}
	if second.InstanceID != first.InstanceID {
		t.Errorf("instance_id changed between probes of one process (%q then %q) — "+
			"every probe would look like a restart", first.InstanceID, second.InstanceID)
	}

	// Wrong method and unauthenticated access are both refused. The id is not a
	// secret, but an unauthenticated probe endpoint is still a probe endpoint.
	if rec := httptest.NewRecorder(); true {
		handleDashboardInstance(rec, dashboardRequest(http.MethodPost, "/api/instance", ""))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST /api/instance = %d, want 405", rec.Code)
		}
	}
	unauth := httptest.NewRecorder()
	handleDashboardInstance(unauth, httptest.NewRequest(http.MethodGet, "/api/instance", nil))
	if unauth.Code == http.StatusOK {
		t.Error("unauthenticated /api/instance must not be served")
	}
}

// TestTerminalRestartReattachAssets pins the client half end to end: the
// watcher module ships embedded (an import that 404s takes the whole terminal
// module graph with it), the terminal transport is the thing that uses it, and
// the endpoint string agrees with the route registered above.
//
// Behaviour — what is polled, for how long, and which disconnects qualify — is
// covered by the jstest suites instance-watch and terminal-restart-reattach.
func TestTerminalRestartReattachAssets(t *testing.T) {
	watch := string(mustReadFS(dashboardAssetsFS, "js/instance-watch.js"))
	for _, want := range []string{
		"'/api/instance'",
		"export function createRestartWatcher",
		"watchForRestart",
	} {
		if !strings.Contains(watch, want) {
			t.Errorf("instance-watch.js missing %q", want)
		}
	}

	core := string(mustReadFS(dashboardAssetsFS, "js/terminals-core.js"))
	for _, want := range []string{
		"from './instance-watch.js'",
		"restartWatcher.watchForRestart(",
		"restartWatcher.currentID()",
	} {
		if !strings.Contains(core, want) {
			t.Errorf("terminals-core.js missing %q — the terminal no longer repairs itself "+
				"after an agentd restart", want)
		}
	}
}
