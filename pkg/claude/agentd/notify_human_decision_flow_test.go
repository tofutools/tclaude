package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// An agent puts a decision to the operator; the operator picks an option
// (even after the agent went offline) and the pick comes back as mail.
func TestHumanDecision_PickComesBackAsMail(t *testing.T) {
	f := newFlow(t)
	const sender = "hdec-aaaa-bbbb-cccc-000000000001"
	f.HaveGroup("design")
	f.HaveMember("design", sender)
	f.HaveAliveSession(sender, "hdec", "tclaude-hdec", f.TestCwd("work"))
	require.NoError(t, db.GrantAgentPermission(sender, agentd.PermHumanNotify, "test"))

	notify := func(body map[string]any) *httptest.ResponseRecorder {
		r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human", body), sender)
		return testharness.Serve(f.Mux, r)
	}
	for _, bad := range []map[string]any{
		{"body": "x", "options": []string{"only one"}},
		{"body": "x", "options": []string{"a", "a"}},
		{"body": "x", "options": []string{"a", "two\nlines"}},
		{"body": "x", "options": []string{"a", "b"}, "recommended": 3},
		{"body": "x", "options": []string{"a", "b", "c", "d", "e", "f", "g"}},
		{"body": "x", "recommended": 1},
	} {
		rec := notify(bad)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%v → %s", bad, rec.Body.String())
	}
	rec := notify(map[string]any{
		"subject": "Which layout for the brief?", "body": "Tabs keep it compact; a sidebar shows more at once.",
		"options": []string{" Tabs ", "Sidebar"}, "recommended": 2,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	// The dashboard snapshot carries the decision for the companion card.
	mux := dashMessageMux(t)
	snap := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, snap.Code)
	var s struct {
		Messages []struct {
			ID          int64    `json:"id"`
			Options     []string `json:"options"`
			Recommended int      `json:"recommended"`
			Decision    string   `json:"decision"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(snap.Body.Bytes(), &s))
	require.NotEmpty(t, s.Messages)
	require.Equal(t, created.ID, s.Messages[0].ID)
	require.Equal(t, []string{"Tabs", "Sidebar"}, s.Messages[0].Options, "options are trimmed")
	require.Equal(t, 2, s.Messages[0].Recommended)
	require.Empty(t, s.Messages[0].Decision)

	// The agent's pane closed; the operator still answers.
	f.MarkOffline("tclaude-hdec")
	require.Equal(t, http.StatusBadRequest, postDashReply(t, mux, map[string]any{"id": created.ID, "choice": 3}).Code)
	reply := postDashReply(t, mux, map[string]any{"id": created.ID, "choice": 2, "body": "and keep it narrow"})
	require.Equal(t, http.StatusOK, reply.Code, reply.Body.String())

	rows, err := db.ListAgentMessagesForConv(sender, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1, "queued for the offline agent")
	assert.True(t, strings.HasPrefix(rows[0].Body, "Decision: Sidebar\n"), rows[0].Body)
	assert.Contains(t, rows[0].Body, "Which layout for the brief?")
	assert.Contains(t, rows[0].Body, "and keep it narrow")

	orig, err := db.GetHumanMessage(created.ID)
	require.NoError(t, err)
	assert.Equal(t, "Sidebar", orig.Decision)
	assert.True(t, orig.IsRead())
	assert.False(t, orig.DecidedAt.IsZero())

	// Only the first answer counts.
	again := postDashReply(t, mux, map[string]any{"id": created.ID, "choice": 1})
	require.Equal(t, http.StatusConflict, again.Code, again.Body.String())

	// A plain notification is not a decision.
	plain, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: sender, Body: "fyi"})
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, postDashReply(t, mux, map[string]any{"id": plain, "choice": 1}).Code)
}
