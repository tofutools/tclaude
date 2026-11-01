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
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The Messages reader reads the mailbox route, not the snapshot, so a decision
// must come through it whole: unanswered, then with the pick. With Collab off a
// stored decision reads as a plain notification there too.
func TestHumanDecision_MailboxCarriesIt(t *testing.T) {
	f := newFlow(t)
	enableCollab(t)
	const sender = "hdec-aaaa-bbbb-cccc-000000000002"
	f.HaveGroup("design")
	f.HaveMember("design", sender)
	f.HaveAliveSession(sender, "hdec", "tclaude-hdec", f.TestCwd("work"))
	require.NoError(t, db.GrantAgentPermission(sender, agentd.PermHumanNotify, "test"))
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human", map[string]any{
		"subject": "Layout?", "body": "Pick one.", "options": []string{"Tabs", "Sidebar"}, "recommended": 2,
	}), sender))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	type row struct {
		ID          int64    `json:"id"`
		Options     []string `json:"options"`
		Recommended int      `json:"recommended"`
		Decision    string   `json:"decision"`
	}
	mux := dashMessageMux(t)
	read := func() row {
		t.Helper()
		page := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/mailbox?id=human", nil))
		require.Equal(t, http.StatusOK, page.Code, page.Body.String())
		var p struct {
			Messages []row `json:"messages"`
		}
		require.NoError(t, json.Unmarshal(page.Body.Bytes(), &p))
		require.Len(t, p.Messages, 1)
		return p.Messages[0]
	}
	got := read()
	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, []string{"Tabs", "Sidebar"}, got.Options)
	assert.Equal(t, 2, got.Recommended)
	assert.Empty(t, got.Decision)

	reply := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/human-messages/reply", map[string]any{"id": created.ID, "choice": 1}))
	require.Equal(t, http.StatusOK, reply.Code, reply.Body.String())
	assert.Equal(t, "Tabs", read().Decision)

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features.Collab = false
	require.NoError(t, config.Save(cfg))
	assert.Empty(t, read().Options, "with Collab off it is a plain notification")
}

// An agent puts a decision to the operator; the operator picks an option
// (even after the agent went offline) and the pick comes back as mail.
func TestHumanDecision_PickComesBackAsMail(t *testing.T) {
	f := newFlow(t)
	enableCollab(t)
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
		{"body": "x", "options": []string{"a", "two\u2028lines"}},
		{"body": "x", "options": []string{"a", "two\u2029paragraphs"}},
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

	// A pick racing another view's pick loses at the claim and sends nothing.
	raced, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: sender, Body: "race?", Options: []string{"a", "b"}})
	require.NoError(t, err)
	claimed, err := db.DecideHumanMessage(raced, "a")
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, db.UndecideHumanMessage(raced))
	claimed, err = db.DecideHumanMessage(raced, "b")
	require.NoError(t, err)
	require.True(t, claimed, "a released decision can be answered again")
	before, err := db.ListAgentMessagesForConv(sender, 100)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, postDashReply(t, mux, map[string]any{"id": raced, "choice": 1}).Code)
	after, err := db.ListAgentMessagesForConv(sender, 100)
	require.NoError(t, err)
	require.Len(t, after, len(before), "the losing pick is not mailed")

	// Only an agent can ask: nobody would get the human's own pick back.
	human := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human",
		map[string]any{"body": "x", "options": []string{"a", "b"}})))
	require.Equal(t, http.StatusBadRequest, human.Code, human.Body.String())

	// A plain notification is not a decision.
	plain, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: sender, Body: "fyi"})
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, postDashReply(t, mux, map[string]any{"id": plain, "choice": 1}).Code)
}

// With Collab off a stored decision reads as a plain notification and cannot
// be answered; turning it back on brings the buttons back.
func TestHumanDecision_HiddenWithCollabOff(t *testing.T) {
	f := newFlow(t)
	enableCollab(t)
	const sender = "hdof-aaaa-bbbb-cccc-000000000001"
	f.HaveGroup("design")
	f.HaveMember("design", sender)
	id, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: sender, Body: "which?", Options: []string{"a", "b"}, Recommended: 2})
	require.NoError(t, err)
	_, err = db.MarkHumanMessageRead(id)
	require.NoError(t, err)

	mux := dashMessageMux(t)
	optionsInSnapshot := func() (riding bool, options []string, recommended int) {
		snap := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
		require.Equal(t, http.StatusOK, snap.Code)
		var s struct {
			Messages []struct {
				ID          int64    `json:"id"`
				Options     []string `json:"options"`
				Recommended int      `json:"recommended"`
			} `json:"messages"`
		}
		require.NoError(t, json.Unmarshal(snap.Body.Bytes(), &s))
		for _, m := range s.Messages {
			if m.ID == id {
				return true, m.Options, m.Recommended
			}
		}
		return false, nil, 0
	}
	riding, options, recommended := optionsInSnapshot()
	require.True(t, riding)
	require.Equal(t, []string{"a", "b"}, options)
	require.Equal(t, 2, recommended)

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features.Collab = false
	require.NoError(t, config.Save(cfg))
	_, options, recommended = optionsInSnapshot()
	assert.Empty(t, options)
	assert.Zero(t, recommended)
	reply := postDashReply(t, mux, map[string]any{"id": id, "choice": 1})
	require.Equal(t, http.StatusNotFound, reply.Code, reply.Body.String())
	assert.Contains(t, reply.Body.String(), "collab_disabled")
	orig, err := db.GetHumanMessage(id)
	require.NoError(t, err)
	assert.Empty(t, orig.Decision)
}

// A pick that cannot be delivered is released whole: the decision can be
// answered again, and it is unread again if it was unread.
func TestHumanDecision_FailedDeliveryRestoresUnread(t *testing.T) {
	f := newFlow(t)
	enableCollab(t)
	const sender = "hdfl-aaaa-bbbb-cccc-000000000001"
	f.HaveGroup("design")
	f.HaveMember("design", sender)
	f.HaveAliveSession(sender, "hdfl", "tclaude-hdfl", f.TestCwd("work"))
	seedRegularBacklog(t, sender, regularMessageQueueLimitForTest)
	mux := dashMessageMux(t)

	unread, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: sender, Body: "unread?", Options: []string{"a", "b"}})
	require.NoError(t, err)
	seen, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: sender, Body: "seen?", Options: []string{"a", "b"}})
	require.NoError(t, err)
	_, err = db.MarkHumanMessageRead(seen)
	require.NoError(t, err)

	for id, wantRead := range map[int64]bool{unread: false, seen: true} {
		reply := postDashReply(t, mux, map[string]any{"id": id, "choice": 1})
		require.NotEqual(t, http.StatusOK, reply.Code, reply.Body.String())
		orig, err := db.GetHumanMessage(id)
		require.NoError(t, err)
		assert.Empty(t, orig.Decision)
		assert.Equal(t, wantRead, orig.IsRead())
	}
}
