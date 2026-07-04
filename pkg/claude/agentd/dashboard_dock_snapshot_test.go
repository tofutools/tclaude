package agentd

import (
	"encoding/json"
	"strings"
	"testing"
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
