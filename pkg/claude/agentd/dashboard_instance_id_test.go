package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSnapshotCarriesStableDaemonInstanceID pins the restart marker connected
// pages use to notice that the agentd they were talking to is gone.
//
// A browser terminal's WebSocket dies with the daemon, but the dashboard's
// 2s/10s poll can easily step straight over a quick restart without a single
// refused request — so "did any poll fail?" is not a sufficient restart
// signal. instance_id is: it is constant for the life of one process and
// necessarily different in the next one. Version cannot serve here, because
// restarting the same build does not change it.
func TestSnapshotCarriesStableDaemonInstanceID(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	get := func() snapshotPayload {
		t.Helper()
		rec := httptest.NewRecorder()
		handleDashboardSnapshot(rec, dashboardRequest(http.MethodGet, "/api/snapshot", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("snapshot status = %d, body=%s", rec.Code, rec.Body.String())
		}
		var snapshot snapshotPayload
		if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
			t.Fatalf("decode snapshot: %v", err)
		}
		return snapshot
	}

	first := get()
	if first.InstanceID == "" {
		t.Fatal("snapshot instance_id is empty — a page can never detect an agentd restart")
	}
	if second := get(); second.InstanceID != first.InstanceID {
		t.Errorf("instance_id changed between polls of one process (%q then %q) — "+
			"every poll would look like a restart", first.InstanceID, second.InstanceID)
	}
	if first.InstanceID == first.Version {
		t.Error("instance_id must identify the process, not the build")
	}
}
