package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: the dashboard's `+ add member` overlay POSTs to
// `/api/groups/{name}/members` with a conv-id picked from the
// snapshot's `ungrouped[]` candidate list. The next snapshot must
// show the conv as a member of the group AND drop it from
// `ungrouped[]`.
//
// Pins the read/write loop the overlay relies on:
//   - snapshot surfaces the candidate (already covered by
//     dashboard_ungrouped_flow_test, but re-asserted here so this
//     test fails for the right reason if the cookie-auth POST never
//     wires through);
//   - POST /api/groups/{name}/members succeeds via dashboard cookie
//     auth (NOT via /v1 SO_PEERCRED — the browser can't speak that);
//   - the next snapshot reflects both sides of the move.
func TestDashboardAddMember_FromUngrouped(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const looseConv = "loos-aaaa-bbbb-cccc-dddd"
	f.HaveConvWithTitle(looseConv, "loose-worker")
	f.HaveAliveSession(looseConv, "spwn-loose", "tmux-loose", "/tmp/loose")
	f.HaveGroup("alpha")

	mux := agentd.BuildDashboardHandlerForTest()

	// Pre-condition: loose conv shows up in ungrouped[] so the
	// candidate list would surface it.
	pre := fetchDashSnapshot(t, mux)
	if !containsConv(pre.Ungrouped, looseConv) {
		t.Fatalf("pre-add: loose conv %s should appear in ungrouped[]; got %d rows",
			looseConv, len(pre.Ungrouped))
	}

	// Add via the dashboard endpoint (the POST the overlay fires).
	body := strings.NewReader(`{"conv":"` + looseConv + `"}`)
	r, err := http.NewRequest(http.MethodPost, "/api/groups/alpha/members", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	r.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/groups/alpha/members status=%d body=%s", rec.Code, rec.Body.String())
	}
	var addResp struct {
		ConvID string `json:"conv_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &addResp); err != nil {
		t.Fatalf("decode add response: %v", err)
	}
	if addResp.ConvID != looseConv {
		t.Errorf("response conv_id = %q, want %q", addResp.ConvID, looseConv)
	}

	// Post-condition: snapshot now has the conv under group alpha
	// AND drops it from ungrouped[].
	post := fetchDashSnapshot(t, mux)
	if containsConv(post.Ungrouped, looseConv) {
		t.Errorf("post-add: loose conv %s should NOT be in ungrouped[] after joining alpha", looseConv)
	}
	postAgent := findAgent(post.Agents, looseConv)
	if postAgent == nil {
		t.Fatalf("post-add: loose conv %s missing from agents[]", looseConv)
	}
	if !containsString(postAgent.Groups, "alpha") {
		t.Errorf("post-add: agent groups = %v; want includes \"alpha\"", postAgent.Groups)
	}
}

// Scenario: re-adding the same conv via the overlay is idempotent —
// the daemon's INSERT OR REPLACE guard means a re-click after the
// optimistic UI lag returns 200 without creating a duplicate row.
// Pins the overlay's "user clicks twice through the lag" path: if
// this stops being idempotent the overlay would surface a confusing
// failure toast on what looked like a fresh add.
func TestDashboardAddMember_RepeatIsIdempotent(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const conv = "dup-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "dup-worker")
	f.HaveAliveSession(conv, "spwn-dup", "tmux-dup", "/tmp/dup")
	f.HaveGroup("alpha")

	mux := agentd.BuildDashboardHandlerForTest()

	doAdd := func() *http.Request {
		body := strings.NewReader(`{"conv":"` + conv + `"}`)
		r, err := http.NewRequest(http.MethodPost, "/api/groups/alpha/members", body)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		r.Header.Set("Content-Type", "application/json")
		return r
	}

	first := testharness.Serve(mux, doAdd())
	if first.Code != http.StatusOK {
		t.Fatalf("first add status=%d body=%s", first.Code, first.Body.String())
	}
	second := testharness.Serve(mux, doAdd())
	if second.Code != http.StatusOK {
		t.Fatalf("second add status=%d body=%s; want 200 (idempotent)", second.Code, second.Body.String())
	}
	// Membership row count: still exactly one across the snapshot's
	// per-group view, despite two adds.
	count := countMembersInGroup(t, mux, "alpha", conv)
	if count != 1 {
		t.Errorf("membership rows for %s in alpha = %d; want 1", conv, count)
	}
}

// Scenario: an unknown conv-id (no resolver match) returns 404 from
// the dashboard endpoint, mirroring /v1 behavior. This keeps the
// overlay's error surface readable when a stale candidate row gets
// clicked after a delete.
func TestDashboardAddMember_UnknownConvReturns404(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)
	f.HaveGroup("alpha")

	mux := agentd.BuildDashboardHandlerForTest()
	body := strings.NewReader(`{"conv":"no-such-conv-anywhere"}`)
	r, err := http.NewRequest(http.MethodPost, "/api/groups/alpha/members", body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	r.Header.Set("Content-Type", "application/json")
	rec := testharness.Serve(mux, r)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status=%d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- helpers ---

func containsConv(rows []dashAgent, conv string) bool {
	for _, a := range rows {
		if a.ConvID == conv {
			return true
		}
	}
	return false
}

func findAgent(rows []dashAgent, conv string) *dashAgent {
	for i := range rows {
		if rows[i].ConvID == conv {
			return &rows[i]
		}
	}
	return nil
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// countMembersInGroup walks the snapshot's per-group members view (the
// same path the dashboard's `+ add member` overlay reads from) to
// count how many rows reference convID inside a given group. Used by
// the idempotent-add scenario to confirm INSERT OR REPLACE doesn't
// duplicate rows.
func countMembersInGroup(t *testing.T, mux http.Handler, group, convID string) int {
	t.Helper()
	r := testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil)
	rec := testharness.Serve(mux, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/snapshot status=%d body=%s", rec.Code, rec.Body.String())
	}
	var snap struct {
		Groups []struct {
			Name    string `json:"name"`
			Members []struct {
				ConvID string `json:"conv_id"`
			} `json:"members"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	count := 0
	for _, g := range snap.Groups {
		if g.Name != group {
			continue
		}
		for _, m := range g.Members {
			if m.ConvID == convID {
				count++
			}
		}
	}
	return count
}
