package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

type fleetBriefResp struct {
	Counts struct {
		Agents, Online, Working, Idle, Offline int
		NeedsYou                               int `json:"needs_you"`
	} `json:"counts"`
	Attention []struct {
		Kind    string `json:"kind"`
		AgentID string `json:"agent_id"`
		Title   string `json:"title"`
		Detail  string `json:"detail"`
		URL     string `json:"url"`
	} `json:"attention"`
	Agents []struct {
		AgentID string   `json:"agent_id"`
		Title   string   `json:"title"`
		Groups  []string `json:"groups"`
		Online  bool     `json:"online"`
		Status  string   `json:"status"`
		Task    string   `json:"task"`
		PRs     []struct {
			URL string `json:"url"`
		} `json:"prs"`
	} `json:"agents"`
	UnreadMessages int `json:"unread_messages"`
}

// The Steward sees the whole fleet, not just its own scribe group, and the
// brief ranks what needs the operator.
func TestFleetBriefSeesWholeFleetAndRanksAttention(t *testing.T) {
	f := newFlow(t)
	enableCollab(t)
	f.HaveGroup("steward")
	f.HaveConvWithTitle("steward-conv", "steward")
	f.HaveMember("steward", "steward-conv")
	f.HaveGroup("api")
	for conv, title := range map[string]string{"asker": "asker", "idler": "idler", "builder": "builder"} {
		f.HaveConvWithTitle(conv, title)
		f.HaveMember("api", conv)
		f.HaveAliveSession(conv, title, "tmux-"+conv, t.TempDir())
	}
	f.SetSessionStatus("asker", session.StatusAwaitingPermission)
	f.SetSessionStatus("builder", session.StatusWorking)
	f.SetSessionStatus("idler", session.StatusIdle)
	rows, err := db.FindSessionsByConvID("idler")
	require.NoError(t, err)
	for _, r := range rows {
		r.LastHook = time.Now().Add(-40 * time.Minute)
		require.NoError(t, db.SaveSession(r))
	}
	f.HaveConvWithTitle("solo", "solo")
	f.HaveEnrolledAgent("solo")

	builderID, err := db.AgentIDForConv("builder")
	require.NoError(t, err)
	_, err = db.SetAgentTaskRef(builderID, "https://example.com/tcl-1", "tcl-1")
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(builderID, "https://github.com/o/r/pull/7", "fleet brief", "open")
	require.NoError(t, err)
	_, err = db.InsertHumanMessage(&db.HumanMessage{Body: "done"})
	require.NoError(t, err)
	// One access request waits on the operator; one was already decided and
	// must not show up as waiting.
	_, stop := agentd.SeedApprovalWithWaiterForTest("req-pending", "self.rename", "idler", false)
	t.Cleanup(stop)
	require.NoError(t, db.UpsertAccessRequest(&db.AccessRequest{ID: "req-done", Perm: "self.rename", ConvID: "asker", Status: "approved", CreatedAt: time.Now(), DecidedAt: time.Now()}))

	require.Equal(t, http.StatusForbidden, accountQuery(t, f, "steward-conv", "/v1/fleet").Code)
	require.NoError(t, db.GrantAgentPermission("steward-conv", agentd.PermFleetRead, "test"))

	rec := accountQuery(t, f, "steward-conv", "/v1/fleet")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var brief fleetBriefResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &brief))

	titles := map[string]bool{}
	for _, a := range brief.Agents {
		titles[a.Title] = true
		if a.Title == "builder" {
			require.Equal(t, []string{"api"}, a.Groups)
			require.Equal(t, "https://example.com/tcl-1", a.Task)
			require.Len(t, a.PRs, 1)
		}
	}
	require.Equal(t, map[string]bool{"asker": true, "idler": true, "builder": true, "solo": true}, titles,
		"every agent outside the caller's groups is listed, the caller itself is not")
	require.Equal(t, 4, brief.Counts.Agents)
	require.Equal(t, 3, brief.Counts.Online)
	require.Equal(t, 1, brief.Counts.NeedsYou)
	require.Equal(t, 1, brief.Counts.Working)
	require.Equal(t, 1, brief.Counts.Idle)
	require.Equal(t, 1, brief.Counts.Offline)
	require.Equal(t, 1, brief.UnreadMessages)

	var kinds []string
	for _, a := range brief.Attention {
		kinds = append(kinds, a.Kind+":"+a.Title)
	}
	require.Equal(t, []string{"approval:idler", "prompt:asker", "pr:builder", "messages:", "idle:idler"}, kinds,
		"ranked most urgent first; a handled request is not listed")
	for _, a := range brief.Agents {
		if a.Title == "solo" {
			require.False(t, a.Online)
			require.Equal(t, "offline", a.Status)
		}
	}

	// A group filter narrows to that group and leaves out fleet-wide items.
	rec = accountQuery(t, f, "steward-conv", "/v1/fleet?group=api")
	require.Equal(t, http.StatusOK, rec.Code)
	brief = fleetBriefResp{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &brief))
	require.Equal(t, 3, brief.Counts.Agents)
	require.Equal(t, 0, brief.UnreadMessages)
	require.Equal(t, "approval", brief.Attention[0].Kind, "a member's pending request stays under a group filter")
	require.Equal(t, http.StatusNotFound, accountQuery(t, f, "steward-conv", "/v1/fleet?group=nope").Code)
}
