package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These tests exercise the dashboard mail client's read surfaces — the
// cookie-authed GET /api/mailboxes (the sidebar roster) and GET
// /api/mailbox?id=<conv|human> (a folder's messages). They are the
// browser twins of the data `tclaude agent inbox` reads, so the
// assertions hit the real production handlers (handleDashboardMailboxes
// / handleDashboardMailbox) through the dashboard mux, exactly as the
// JS does.

const (
	mbAlice = "mbox-alic-1111-2222-333333333301"
	mbBob   = "mbox-bobb-1111-2222-333333333302"
)

// mailboxEntry mirrors the dashboardMailbox wire shape.
type mailboxEntry struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"`
	Title  string   `json:"title"`
	Online bool     `json:"online"`
	Groups []string `json:"groups"`
	In     int      `json:"in"`
	Out    int      `json:"out"`
	Total  int      `json:"total"`
	Unread int      `json:"unread"`
	LastAt string   `json:"last_at"`
}

// mailboxMsg mirrors the mailboxMessage wire shape (subset the tests
// assert on).
type mailboxMsg struct {
	ID        int64  `json:"id"`
	Direction string `json:"direction"`
	FromConv  string `json:"from_conv"`
	FromTitle string `json:"from_title"`
	ToConv    string `json:"to_conv"`
	ToTitle   string `json:"to_title"`
	Group     string `json:"group"`
	Subject   string `json:"subject"`
	Body      string `json:"body"`
	Read      bool   `json:"read"`
}

// seedMailboxes stands up two named agents in a group with one message
// each direction (alice→bob unread, bob→alice read) plus two human
// notifications (one unread). Returns the dashboard handler.
func seedMailboxes(t *testing.T, f *testharness.Flow) http.Handler {
	t.Helper()
	g := f.HaveGroup("team")
	f.HaveMember("team", mbAlice)
	f.HaveMember("team", mbBob)
	f.HaveConvWithTitle(mbAlice, "alice")
	f.HaveConvWithTitle(mbBob, "bob")

	base := time.Now().Add(-time.Hour)
	// alice → bob, unread.
	_, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g.ID, FromConv: mbAlice, ToConv: mbBob,
		Subject: "hi bob", Body: "first contact", CreatedAt: base,
	})
	require.NoError(t, err)
	// bob → alice, read.
	_, err = db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g.ID, FromConv: mbBob, ToConv: mbAlice,
		Subject: "re: hi bob", Body: "got it", CreatedAt: base.Add(time.Minute),
		ReadAt: base.Add(2 * time.Minute),
	})
	require.NoError(t, err)

	hm1, err := db.InsertHumanMessage(&db.HumanMessage{FromConv: mbAlice, FromTitle: "alice", Body: "human note one"})
	require.NoError(t, err)
	_, err = db.InsertHumanMessage(&db.HumanMessage{FromConv: mbBob, FromTitle: "bob", Body: "human note two"})
	require.NoError(t, err)
	// Mark one of the two read so the human folder reports exactly one
	// unread (the badge-driving count).
	_, err = db.MarkHumanMessageRead(hm1)
	require.NoError(t, err)

	return dashHandlerForTest(t)
}

func getMailboxes(t *testing.T, dash http.Handler) []mailboxEntry {
	t.Helper()
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, "/api/mailboxes", nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var payload struct {
		Mailboxes []mailboxEntry `json:"mailboxes"`
	}
	testharness.DecodeJSON(t, rec, &payload)
	return payload.Mailboxes
}

func findMailbox(boxes []mailboxEntry, id string) *mailboxEntry {
	for i := range boxes {
		if boxes[i].ID == id {
			return &boxes[i]
		}
	}
	return nil
}

// Scenario: /api/mailboxes enumerates the human folder (pinned first)
// plus one folder per agent, each carrying the right in/out/unread
// tallies derived from agent_messages.
func TestDashboardMailboxes_EnumeratesFoldersWithCounts(t *testing.T) {
	f := newFlow(t)
	dash := seedMailboxes(t, f)

	boxes := getMailboxes(t, dash)
	require.GreaterOrEqual(t, len(boxes), 2)

	// The virtual "all" firehose leads, then the human.notify channel.
	assert.Equal(t, "all", boxes[0].ID)
	assert.Equal(t, "human", boxes[1].ID)
	assert.Equal(t, "human", boxes[1].Kind)
	assert.Equal(t, 2, boxes[1].Total, "two human notifications")
	assert.Equal(t, 1, boxes[1].Unread, "one of them unread drives the badge")

	alice := findMailbox(boxes, mbAlice)
	require.NotNil(t, alice, "alice has a mailbox")
	assert.Equal(t, "agent", alice.Kind)
	assert.Equal(t, "alice", alice.Title)
	assert.Equal(t, 1, alice.In, "alice received bob's reply")
	assert.Equal(t, 1, alice.Out, "alice sent one")
	assert.Equal(t, 2, alice.Total)
	assert.Equal(t, 0, alice.Unread, "alice read bob's reply")
	assert.Contains(t, alice.Groups, "team")

	bob := findMailbox(boxes, mbBob)
	require.NotNil(t, bob, "bob has a mailbox")
	assert.Equal(t, "bob", bob.Title)
	assert.Equal(t, 1, bob.In)
	assert.Equal(t, 1, bob.Out)
	assert.Equal(t, 1, bob.Unread, "bob has not read alice's message")
}

// Scenario: a freshly-enrolled agent with no mail still gets a folder
// (so the human can pick it to send / inspect), with zero counts.
func TestDashboardMailboxes_EmptyAgentStillListed(t *testing.T) {
	f := newFlow(t)
	const lonely = "mbox-solo-1111-2222-333333333309"
	f.HaveGroup("team")
	f.HaveMember("team", lonely)
	f.HaveConvWithTitle(lonely, "solo")
	dash := dashHandlerForTest(t)

	boxes := getMailboxes(t, dash)
	mb := findMailbox(boxes, lonely)
	require.NotNil(t, mb, "an agent with an empty mailbox is still listed")
	assert.Equal(t, 0, mb.Total)
	assert.Equal(t, 0, mb.Unread)
}

// Scenario: /api/mailbox?id=<conv> returns the folder's received + sent
// messages merged newest-first, with directions and counterpart titles
// resolved for the reading pane.
func TestDashboardMailbox_AgentFolderMergesInAndOut(t *testing.T) {
	f := newFlow(t)
	dash := seedMailboxes(t, f)

	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet,
		"/api/mailbox?id="+mbBob, nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var payload struct {
		ID       string       `json:"id"`
		Kind     string       `json:"kind"`
		Title    string       `json:"title"`
		Messages []mailboxMsg `json:"messages"`
	}
	testharness.DecodeJSON(t, rec, &payload)
	assert.Equal(t, mbBob, payload.ID)
	assert.Equal(t, "bob", payload.Title)
	require.Len(t, payload.Messages, 2, "bob's mailbox holds the one he received + the one he sent")

	// Newest first: bob's reply (sent) precedes alice's opener (received).
	assert.Equal(t, "out", payload.Messages[0].Direction)
	assert.Equal(t, "re: hi bob", payload.Messages[0].Subject)
	assert.Equal(t, "alice", payload.Messages[0].ToTitle, "outbound row resolves the recipient")

	assert.Equal(t, "in", payload.Messages[1].Direction)
	assert.Equal(t, "hi bob", payload.Messages[1].Subject)
	assert.Equal(t, "alice", payload.Messages[1].FromTitle, "inbound row resolves the sender")
	assert.False(t, payload.Messages[1].Read, "alice's opener is unread")
	assert.Equal(t, "team", payload.Messages[1].Group)
}

// Scenario: id=human returns the human_messages folder, every row
// direction "in" (agents → human).
func TestDashboardMailbox_HumanFolder(t *testing.T) {
	f := newFlow(t)
	dash := seedMailboxes(t, f)

	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet,
		"/api/mailbox?id=human", nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var payload struct {
		ID       string       `json:"id"`
		Kind     string       `json:"kind"`
		Messages []mailboxMsg `json:"messages"`
	}
	testharness.DecodeJSON(t, rec, &payload)
	assert.Equal(t, "human", payload.ID)
	assert.Equal(t, "human", payload.Kind)
	require.Len(t, payload.Messages, 2)
	for _, m := range payload.Messages {
		assert.Equal(t, "in", m.Direction, "human notifications are inbound to the human")
	}
}

// Scenario: a missing id is a 400 — the JS always sends one.
func TestDashboardMailbox_MissingID_BadRequest(t *testing.T) {
	newFlow(t)
	dash := dashHandlerForTest(t)
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, "/api/mailbox", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}

// Scenario: the endpoints refuse an uncookied request — the dashboard
// auth gate is actually wired (BuildDashboardHandlerForTest injects the
// cookie; the raw mux must not).
func TestDashboardMailbox_RequiresAuth(t *testing.T) {
	newFlow(t)
	mux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(mux)
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/mailboxes", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code, "uncookied request is refused")
}

// getMailbox fetches one folder's messages through the production
// handler — the read surface the delete/wipe tests assert against.
func getMailbox(t *testing.T, dash http.Handler, id string) []mailboxMsg {
	t.Helper()
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet,
		"/api/mailbox?id="+id, nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var payload struct {
		Messages []mailboxMsg `json:"messages"`
	}
	testharness.DecodeJSON(t, rec, &payload)
	return payload.Messages
}

// Scenario: the roster leads with the virtual "all" folder, whose total
// is the DISTINCT agent_messages row count (two rows here), not the
// In+Out sum the per-conv tallies report.
func TestDashboardMailboxes_AllVirtualFolderLeads(t *testing.T) {
	f := newFlow(t)
	dash := seedMailboxes(t, f)

	boxes := getMailboxes(t, dash)
	require.GreaterOrEqual(t, len(boxes), 2)
	assert.Equal(t, "all", boxes[0].ID, "the all-messages firehose is pinned first")
	assert.Equal(t, "all", boxes[0].Kind)
	assert.Equal(t, 2, boxes[0].Total, "two distinct agent_messages rows")
	assert.Equal(t, 0, boxes[0].Unread, "the aggregate has no per-recipient unread")
	// Human folder follows the virtual one.
	assert.Equal(t, "human", boxes[1].ID)
}

// Scenario: id=all returns every agent_messages row newest-first, each
// carrying both ends resolved so the firehose can render from→to.
func TestDashboardMailbox_AllFolderListsEverythingNewestFirst(t *testing.T) {
	f := newFlow(t)
	dash := seedMailboxes(t, f)

	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet,
		"/api/mailbox?id=all", nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var payload struct {
		ID       string       `json:"id"`
		Kind     string       `json:"kind"`
		Messages []mailboxMsg `json:"messages"`
	}
	testharness.DecodeJSON(t, rec, &payload)
	assert.Equal(t, "all", payload.ID)
	assert.Equal(t, "all", payload.Kind)
	require.Len(t, payload.Messages, 2)

	// Newest first: bob's reply precedes alice's opener. Both ends are
	// title-resolved for the from→to render.
	assert.Equal(t, "re: hi bob", payload.Messages[0].Subject)
	assert.Equal(t, "bob", payload.Messages[0].FromTitle)
	assert.Equal(t, "alice", payload.Messages[0].ToTitle)
	assert.Equal(t, "hi bob", payload.Messages[1].Subject)
}

// Scenario: POST /api/mailbox/delete removes the named agent_messages
// rows, and the deletion shows up in BOTH parties' folders — it is one
// shared row.
func TestDashboardMailboxDelete_RemovesByIDs(t *testing.T) {
	f := newFlow(t)
	dash := seedMailboxes(t, f)

	// Grab bob's two messages; delete the one he received from alice.
	bobMsgs := getMailbox(t, dash, mbBob)
	require.Len(t, bobMsgs, 2)
	var dropID int64
	for _, m := range bobMsgs {
		if m.Subject == "hi bob" {
			dropID = m.ID
		}
	}
	require.NotZero(t, dropID, "found alice→bob message")

	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/mailbox/delete", map[string]any{"ids": []int64{dropID}}))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		Deleted int `json:"deleted"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	assert.Equal(t, 1, resp.Deleted)

	// Gone from bob's folder (recipient) AND alice's (sender).
	require.Len(t, getMailbox(t, dash, mbBob), 1, "bob's received copy removed")
	for _, m := range getMailbox(t, dash, mbAlice) {
		assert.NotEqual(t, dropID, m.ID, "same shared row gone from alice's folder too")
	}
	require.Len(t, getMailbox(t, dash, "all"), 1, "firehose down to one row")
}

// Scenario: an empty ids list is a 400 — the client must name rows.
func TestDashboardMailboxDelete_RequiresIDs(t *testing.T) {
	f := newFlow(t)
	dash := seedMailboxes(t, f)
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/mailbox/delete", map[string]any{"ids": []int64{}}))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}

// Scenario: POST /api/mailbox/wipe deletes every row where any listed
// conv is a party. Wiping alice removes BOTH messages (she sent one,
// received the other), emptying bob's folder too.
func TestDashboardMailboxWipe_RemovesAllForConvs(t *testing.T) {
	f := newFlow(t)
	dash := seedMailboxes(t, f)

	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/mailbox/wipe", map[string]any{"convs": []string{mbAlice}}))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		Deleted int `json:"deleted"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	assert.Equal(t, 2, resp.Deleted, "both messages touched alice")

	assert.Empty(t, getMailbox(t, dash, mbAlice), "alice's folder emptied")
	assert.Empty(t, getMailbox(t, dash, mbBob), "bob's folder emptied — shared rows")
	assert.Empty(t, getMailbox(t, dash, "all"), "firehose empty")
}

// Scenario: wipe rejects an empty conv list with 400.
func TestDashboardMailboxWipe_RequiresConvs(t *testing.T) {
	f := newFlow(t)
	dash := seedMailboxes(t, f)
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/mailbox/wipe", map[string]any{"convs": []string{}}))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}

// Scenario: the mutating endpoints refuse an uncookied request, same as
// the read surfaces — the human-consent gate is actually wired.
func TestDashboardMailboxMutations_RequireAuth(t *testing.T) {
	newFlow(t)
	mux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(mux)
	del := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
		"/api/mailbox/delete", map[string]any{"ids": []int64{1}}))
	assert.Equal(t, http.StatusForbidden, del.Code, "uncookied delete refused")
	wipe := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
		"/api/mailbox/wipe", map[string]any{"convs": []string{"x"}}))
	assert.Equal(t, http.StatusForbidden, wipe.Code, "uncookied wipe refused")
}
