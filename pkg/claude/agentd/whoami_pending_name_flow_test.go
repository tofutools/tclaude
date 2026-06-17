package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// whoamiTitle drives the real /v1/whoami surface — the endpoint backing
// `tclaude agent whoami` — as the given agent and returns the title the
// agent would read about itself.
func whoamiTitle(t *testing.T, f *testharness.Flow, convID string) string {
	t.Helper()
	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/whoami", nil), convID)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "GET /v1/whoami body=%s", rec.Body.String())
	var resp struct {
		IsHuman bool   `json:"is_human"`
		ConvID  string `json:"conv_id"`
		Title   string `json:"title"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp),
		"decode whoami body=%s", rec.Body.String())
	require.False(t, resp.IsHuman, "agent peer must not classify as human")
	require.Equal(t, convID, resp.ConvID, "whoami conv_id")
	return resp.Title
}

// Scenario (JOH-219): a freshly-spawned agent runs `tclaude agent whoami`
// before any custom title has landed. For Codex this is the normal case —
// its title is persisted out-of-band AFTER the spawn welcome (JOH-216), so
// at whoami-time there is no custom title in conv_index yet. The spawn DID
// record the agent's name as agent_enrollment.pending_name (the `--name`
// arg) before injecting the welcome, so whoami must fall back to it instead
// of reporting "(unnamed)" — otherwise the agent reads itself as unnamed and
// self-describes that way despite the welcome having named it.
//
// Pins the resolution: /v1/whoami goes through agent.FreshTitle (custom →
// pending → summary → first prompt), the same priority the dashboard and
// conv-listing use, rather than a bare DisplayTitle that skips the pending
// name. Harness-agnostic — the gap bit Codex but the fix is one code path.
func TestWhoami_FallsBackToPendingName(t *testing.T) {
	f := newFlow(t)

	const convID = "019ed733-f4df-7510-9514-0e413aabaaf6"
	f.HaveGroup("tclaude-dev")
	f.HaveMember("tclaude-dev", convID) // enrolls the agent
	// Spawn recorded the requested name as the pending name; no custom
	// title exists yet (the out-of-band rename has not landed).
	require.NoError(t, db.SetEnrollmentPendingName(convID, "testc"),
		"SetEnrollmentPendingName")

	got := whoamiTitle(t, f, convID)
	assert.Equal(t, "testc", got,
		"whoami must surface the spawn-time pending name, not (unnamed)")
}

// Guard: a real custom title outranks the pending name (custom is the
// authoritative identity once a rename lands), and an agent with neither a
// custom title nor a pending name still reads as "(unnamed)" — the
// placeholder whoami has always shown for a genuinely unnamed agent.
func TestWhoami_CustomTitleOutranksPending_AndUnnamedFallback(t *testing.T) {
	f := newFlow(t)

	const named = "11111111-1111-1111-1111-111111111111"
	f.HaveGroup("g")
	f.HaveMember("g", named)
	f.HaveConvWithTitle(named, "renamed-title")
	require.NoError(t, db.SetEnrollmentPendingName(named, "stale-pending"),
		"SetEnrollmentPendingName")
	assert.Equal(t, "renamed-title", whoamiTitle(t, f, named),
		"custom title must outrank the pending name")

	const bare = "22222222-2222-2222-2222-222222222222"
	f.HaveMember("g", bare) // enrolled, but no pending name and no title
	assert.Equal(t, "(unnamed)", whoamiTitle(t, f, bare),
		"an agent with no name material reads as (unnamed)")
}
