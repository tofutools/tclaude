package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func snapshotJSON(t *testing.T, mux http.Handler, path string) map[string]any {
	t.Helper()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, path, nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// Scenario: a long federation history. An open proposal stays however old, a
// lapsed one reads expired without the poll writing it, and handled history
// stays behind a small cap.
func TestDashboardSnapshot_ProposalsAreActionablePlusRecent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	d, err := db.Open()
	require.NoError(t, err)
	now := time.Now()
	old := now.AddDate(0, 0, -30).UnixNano()
	proposal := func(id, state string, deadline, updated int64) {
		_, err := d.Exec(`INSERT INTO federation_peer_proposals (id, peer, origin_instance, revision, body, state, created_at, deadline_at, updated_at)
			VALUES (?, 'peer-1', 'peer-1', 1, '{"items":[]}', ?, ?, ?, ?)`, id, state, updated, deadline, updated)
		require.NoError(t, err)
	}
	proposal("prop-live", "sent", now.Add(time.Hour).UnixNano(), old)
	proposal("prop-lapsed", "sent", old, old)
	for i := range 40 {
		proposal(fmt.Sprintf("prop-done-%02d", i), "declined", old, old+int64(i))
	}

	snap := snapshotJSON(t, agentd.BuildDashboardHandlerForTest(), "/api/snapshot")
	status := map[string]string{}
	for _, raw := range snap["access_requests"].([]any) {
		r := raw.(map[string]any)
		status[r["id"].(string)] = r["status"].(string)
	}
	assert.Equal(t, "pending", status["prop-live"], "an open proposal outlives the history cap")
	assert.Equal(t, "expired", status["prop-lapsed"], "a lapsed proposal reads expired before the timer writes it")
	assert.Contains(t, status, "prop-done-39", "the newest handled proposals stay")
	assert.NotContains(t, status, "prop-done-00", "older ones leave the poll")

	row, err := db.GetFederationProposal("prop-lapsed")
	require.NoError(t, err)
	assert.Equal(t, "sent", row.State, "the read path no longer writes on every poll")
}
