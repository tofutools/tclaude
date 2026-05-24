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
	Groups        []dashGroup        `json:"groups"`
	Agents        []dashAgent        `json:"agents"`
	Ungrouped     []dashAgent        `json:"ungrouped"`
	Conversations []dashConversation `json:"conversations"`
	Retired       []dashRetired      `json:"retired"`
	Usage         dashUsage          `json:"usage"`
}

// dashConversation mirrors agentd.dashboardConversation.
type dashConversation struct {
	ConvID string `json:"conv_id"`
	Title  string `json:"title"`
	Online bool   `json:"online"`
}

// dashRetired mirrors agentd.dashboardRetiredAgent.
type dashRetired struct {
	ConvID       string `json:"conv_id"`
	Title        string `json:"title"`
	Online       bool   `json:"online"`
	RetiredBy    string `json:"retired_by,omitempty"`
	RetireReason string `json:"retire_reason,omitempty"`
}

type dashGroup struct {
	Name       string       `json:"name"`
	Descr      string       `json:"descr"`
	MaxMembers int          `json:"max_members"`
	Members    []dashMember `json:"members"`
}

type dashMember struct {
	ConvID        string    `json:"conv_id"`
	Title         string    `json:"title"`
	Branch        string    `json:"branch,omitempty"`
	StartupDir    string    `json:"startup_dir,omitempty"`
	StartupBranch string    `json:"startup_branch,omitempty"`
	CurrentDir    string    `json:"current_dir,omitempty"`
	BranchURL     string    `json:"branch_url,omitempty"`
	BranchPRNum   int       `json:"branch_pr_number,omitempty"`
	BranchPRURL   string    `json:"branch_pr_url,omitempty"`
	BranchPRState string    `json:"branch_pr_state,omitempty"`
	Online        bool      `json:"online"`
	State         dashState `json:"state"`
}

type dashAgent struct {
	ConvID        string    `json:"conv_id"`
	Title         string    `json:"title"`
	Branch        string    `json:"branch,omitempty"`
	StartupDir    string    `json:"startup_dir,omitempty"`
	StartupBranch string    `json:"startup_branch,omitempty"`
	CurrentDir    string    `json:"current_dir,omitempty"`
	BranchURL     string    `json:"branch_url,omitempty"`
	BranchPRNum   int       `json:"branch_pr_number,omitempty"`
	BranchPRURL   string    `json:"branch_pr_url,omitempty"`
	BranchPRState string    `json:"branch_pr_state,omitempty"`
	Online        bool      `json:"online"`
	Groups        []string  `json:"groups"`
	State         dashState `json:"state"`
}

// dashState mirrors the relevant fields of agentd.agentState.
type dashState struct {
	Status            string  `json:"status,omitempty"`
	StatusDetail      string  `json:"status_detail,omitempty"`
	LastHook          string  `json:"last_hook,omitempty"`
	ContextPct        float64 `json:"context_pct,omitempty"`
	TokensInput       int64   `json:"tokens_input,omitempty"`
	TokensOutput      int64   `json:"tokens_output,omitempty"`
	ContextWindowSize int64   `json:"context_window_size,omitempty"`
	ExitReason        string  `json:"exit_reason,omitempty"`
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

// Scenario: an ENROLLED agent has a live tmux session but is NOT a
// member of any group. The dashboard's `/api/snapshot` must surface it
// under the `ungrouped[]` array so the `+ add member` overlay can pull
// it as a candidate without a second fetch.
//
// Pins the bug class where an ungrouped agent is invisible to the
// dashboard until the human manually adds it to a group.
func TestDashboardSnapshot_UngroupedSurfacesLooseConvs(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	// Scenario set-up:
	//   - "loose" — alive, enrolled, not in any group → expect in Ungrouped.
	//   - "joined" — alive, member of group "alpha" (HaveMember enrolls
	//     it) → NOT in Ungrouped.
	const looseConv = "loos-1111-2222-3333-4444"
	const joinedConv = "join-1111-2222-3333-4444"
	f.HaveConvWithTitle(looseConv, "loose-worker")
	f.HaveConvWithTitle(joinedConv, "joined-worker")
	f.HaveAliveSession(looseConv, "spwn-loose", "tmux-loose", "/tmp/loose")
	f.HaveAliveSession(joinedConv, "spwn-join", "tmux-join", "/tmp/join")
	f.HaveEnrolledAgent(looseConv)
	g := f.HaveGroup("alpha")
	_ = g
	f.HaveMember("alpha", joinedConv)

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

// Scenario: ungrouped[] carries offline agents too. The dashboard's
// virtual "Ungrouped" group is a membership-management surface — an
// agent that is enrolled, in no group, and offline must still be
// visible there so the human can drag it into a group. (The `+ add
// member` overlay applies its own online filter, so this does not
// leak offline rows into that live-roster picker.)
func TestDashboardSnapshot_UngroupedIncludesOfflineAgents(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const onlineConv = "onln-1111-2222-3333-4444"
	const offlineConv = "offl-1111-2222-3333-4444"
	f.HaveConvWithTitle(onlineConv, "online")
	f.HaveConvWithTitle(offlineConv, "offline")
	f.HaveAliveSession(onlineConv, "spwn-onln", "tmux-onln", "/tmp/onln")
	f.HaveAliveSession(offlineConv, "spwn-offl", "tmux-offl", "/tmp/offl")
	// Both are enrolled agents; one's tmux has since died.
	f.HaveEnrolledAgent(onlineConv)
	f.HaveEnrolledAgent(offlineConv)
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
	assert.True(t, online, "online ungrouped agent should appear in ungrouped[]; got %d rows", len(snap.Ungrouped))
	assert.True(t, offline, "offline ungrouped agent %s should also appear in ungrouped[]", offlineConv)

	// Both still appear in the broader agents[] list.
	offlineInAgents := false
	for _, a := range snap.Agents {
		if a.ConvID == offlineConv {
			offlineInAgents = true
		}
	}
	assert.True(t, offlineInAgents, "offline enrolled agent %s should still be in agents[]", offlineConv)
}
