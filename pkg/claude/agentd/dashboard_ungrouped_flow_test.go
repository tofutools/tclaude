package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// dashSnapshot mirrors the relevant fields of agentd.snapshotPayload
// without importing the unexported type. Adding fields here is cheap
// when more assertions need them.
type dashSnapshot struct {
	Agents    []dashAgent `json:"agents"`
	Ungrouped []dashAgent `json:"ungrouped"`
}

type dashAgent struct {
	ConvID string   `json:"conv_id"`
	Title  string   `json:"title"`
	Online bool     `json:"online"`
	Groups []string `json:"groups"`
}

func fetchDashSnapshot(t *testing.T, mux http.Handler) dashSnapshot {
	t.Helper()
	r := testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil)
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "/api/snapshot body=%s", rec.Body.String())
	var snap dashSnapshot
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap), "decode snapshot")
	return snap
}

// Scenario: a conv has a live tmux session but is NOT a member of any
// group. The dashboard's `/api/snapshot` must surface it under the
// `ungrouped[]` array so the (eventual) ungrouped virtual group / `+
// add member` overlay can pull it as a candidate without a second
// fetch.
//
// Pins the bug class where a fresh-spawned agent that hasn't joined
// any group yet is invisible to the dashboard until the human
// manually adds it.
func TestDashboardSnapshot_UngroupedSurfacesLooseConvs(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	// Scenario set-up:
	//   - "loose" — alive, not in any group → expect in Ungrouped.
	//   - "joined" — alive, member of group "alpha" → NOT in Ungrouped.
	const looseConv = "loos-1111-2222-3333-4444"
	const joinedConv = "join-1111-2222-3333-4444"
	f.HaveConvWithTitle(looseConv, "loose-worker")
	f.HaveConvWithTitle(joinedConv, "joined-worker")
	f.HaveAliveSession(looseConv, "spwn-loose", "tmux-loose", "/tmp/loose")
	f.HaveAliveSession(joinedConv, "spwn-join", "tmux-join", "/tmp/join")
	g := f.HaveGroup("alpha")
	_ = g
	f.HaveMember("alpha", joinedConv, "joined")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	// Find by conv-id.
	inUngrouped := func(conv string) bool {
		for _, a := range snap.Ungrouped {
			if a.ConvID == conv {
				return true
			}
		}
		return false
	}
	inAgents := func(conv string) bool {
		for _, a := range snap.Agents {
			if a.ConvID == conv {
				return true
			}
		}
		return false
	}

	if !assert.True(t, inUngrouped(looseConv),
		"loose conv %s should be in Ungrouped; got %d ungrouped rows", looseConv, len(snap.Ungrouped)) {
		for _, a := range snap.Ungrouped {
			t.Logf("  ungrouped row: %+v", a)
		}
	}
	assert.False(t, inUngrouped(joinedConv),
		"joined conv %s should NOT be in Ungrouped (it's in group alpha)", joinedConv)
	// Both should appear in the broader Agents list.
	assert.True(t, inAgents(looseConv), "loose conv %s should be in Agents (broader list)", looseConv)
	assert.True(t, inAgents(joinedConv), "joined conv %s should be in Agents (member of alpha)", joinedConv)
}

// Scenario: an offline session row (no live tmux) does NOT pollute
// the ungrouped list. Pins the "stale rows from past runs" filter —
// without this gate, every previously-spawned conv would shore up
// indefinitely as the daemon's history grows.
func TestDashboardSnapshot_UngroupedFiltersOfflineSessions(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const onlineConv = "onln-1111-2222-3333-4444"
	const offlineConv = "offl-1111-2222-3333-4444"
	f.HaveConvWithTitle(onlineConv, "online")
	f.HaveConvWithTitle(offlineConv, "offline")
	f.HaveAliveSession(onlineConv, "spwn-onln", "tmux-onln", "/tmp/onln")
	f.HaveAliveSession(offlineConv, "spwn-offl", "tmux-offl", "/tmp/offl")
	f.MarkOffline("tmux-offl")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	online := false
	offline := false
	for _, a := range snap.Ungrouped {
		if a.ConvID == onlineConv {
			online = true
		}
		if a.ConvID == offlineConv {
			offline = true
		}
	}
	assert.True(t, online, "online ungrouped conv should appear; got %d rows", len(snap.Ungrouped))
	assert.False(t, offline, "offline conv %s should NOT appear in Ungrouped", offlineConv)
}
