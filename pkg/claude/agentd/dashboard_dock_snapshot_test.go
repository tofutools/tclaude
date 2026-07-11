package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestDockSnapshot_CarriesProfilesAndRoles guards the server side of the
// palette dock (JOH-374): the poll snapshot must expose the spawn-profile and
// role registries as JSON arrays (never null — the dock's JS .map() would
// trip), the same way the templates list already rides the poll. A zero-value
// snapshotPayload models "no data yet": the collect* helpers seed empty
// (non-nil) slices, so we set them explicitly here to assert the wire keys +
// the empty-array (not null) shape a real snapshot always emits.
func TestDockSnapshot_CarriesProfilesAndRoles(t *testing.T) {
	s := snapshotPayload{
		Templates: []templateJSON{},
		Profiles:  []spawnProfileJSON{},
		Roles:     []roleJSON{},
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	got := string(b)
	for _, want := range []string{`"profiles":[]`, `"roles":[]`, `"templates":[]`} {
		if !strings.Contains(got, want) {
			t.Errorf("snapshot JSON missing %q (empty registries must serialize as [], not null) — got %s", want, got)
		}
	}
}

func TestDashboardSnapshotCarriesGlobalDefaultSpawnProfile(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)

	profileID, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "global-default"})
	if err != nil {
		t.Fatalf("create spawn profile: %v", err)
	}
	if err := db.SetDashboardProfileRef(
		dashboardDefaultProfilePrefKey, dashboardDefaultProfileIDPrefKey,
		"global-default", profileID,
	); err != nil {
		t.Fatalf("set global default spawn profile: %v", err)
	}

	rec := httptest.NewRecorder()
	handleDashboardSnapshot(rec, dashboardRequest(http.MethodGet, "/api/snapshot", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var snapshot snapshotPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snapshot.SpawnProfileDefault != "global-default" {
		t.Errorf("spawn_profile_default = %q, want global-default", snapshot.SpawnProfileDefault)
	}
}
