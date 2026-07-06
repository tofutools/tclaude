package agentd_test

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
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
	mbCarol = "mbox-caro-1111-2222-333333333303"
	mbDave  = "mbox-dave-1111-2222-333333333304"
	mbEmpty = "mbox-empt-1111-2222-333333333305"
)

// mailboxEntry mirrors the dashboardMailbox wire shape.
type mailboxEntry struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	Title       string   `json:"title"`
	AgentID     string   `json:"agent_id"`
	Online      bool     `json:"online"`
	Retired     bool     `json:"retired"`
	Groups      []string `json:"groups"`
	Members     int      `json:"members"`
	MemberConvs []string `json:"member_convs"`
	In          int      `json:"in"`
	Out         int      `json:"out"`
	Total       int      `json:"total"`
	Unread      int      `json:"unread"`
	LastAt      string   `json:"last_at"`
}

// mailboxMsg mirrors the mailboxMessage wire shape (subset the tests
// assert on).
type mailboxMsg struct {
	ID        int64  `json:"id"`
	Direction string `json:"direction"`
	FromConv  string `json:"from_conv"`
	FromAgent string `json:"from_agent"`
	FromTitle string `json:"from_title"`
	ToConv    string `json:"to_conv"`
	ToAgent   string `json:"to_agent"`
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

	// The sidebar roster carries each agent folder's stable agent_id, resolved
	// live from its conv, so a nameless folder can lead with its agt_ handle
	// (instead of a short conv-id) and hover the full agent_id / conv-id pair.
	// The pinned "all" / "human" folders carry none.
	aliceAgent, err := db.AgentIDForConv(mbAlice)
	require.NoError(t, err)
	require.NotEmpty(t, aliceAgent, "alice is an enrolled agent with a stable id")
	assert.Equal(t, aliceAgent, alice.AgentID, "alice's folder snapshots her stable agent_id")
	bobAgent, err := db.AgentIDForConv(mbBob)
	require.NoError(t, err)
	assert.Equal(t, bobAgent, bob.AgentID, "bob's folder snapshots his stable agent_id")
	assert.Empty(t, boxes[0].AgentID, "the virtual 'all' folder has no agent_id")
	assert.Empty(t, boxes[1].AgentID, "the human folder has no agent_id")
}

// Scenario: a freshly-enrolled agent with no mail is hidden from the
// roster by default (it would be sidebar clutter), but reappears with
// include_empty — with zero counts — so the operator can still inspect it.
func TestDashboardMailboxes_EmptyAgentHiddenUntilOptedIn(t *testing.T) {
	f := newFlow(t)
	const lonely = "mbox-solo-1111-2222-333333333309"
	f.HaveGroup("team")
	f.HaveMember("team", lonely)
	f.HaveConvWithTitle(lonely, "solo")
	dash := dashHandlerForTest(t)

	boxes := getMailboxes(t, dash)
	assert.Nil(t, findMailbox(boxes, lonely),
		"an agent with an empty mailbox is hidden by default")

	boxes = getMailboxesEmptyOpt(t, dash, true)
	mb := findMailbox(boxes, lonely)
	require.NotNil(t, mb, "the empty agent reappears with include_empty")
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
		AgentID  string       `json:"agent_id"`
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

	// JOH-280: each row carries the sender's / recipient's stable agent_id
	// (the snapshot the reading pane leads with) alongside the conv-id, so
	// the web reader can show `name (agt_xxxxxxxx)` with conv on hover.
	aliceAgent, err := db.AgentIDForConv(mbAlice)
	require.NoError(t, err)
	bobAgent, err := db.AgentIDForConv(mbBob)
	require.NoError(t, err)
	require.NotEmpty(t, aliceAgent, "alice is an enrolled agent with a stable id")
	require.NotEmpty(t, bobAgent, "bob is an enrolled agent with a stable id")
	assert.Equal(t, bobAgent, payload.Messages[0].FromAgent, "outbound row snapshots the sender's agent_id")
	assert.Equal(t, aliceAgent, payload.Messages[0].ToAgent, "outbound row snapshots the recipient's agent_id")
	assert.Equal(t, aliceAgent, payload.Messages[1].FromAgent, "inbound row snapshots the sender's agent_id")
	assert.Equal(t, bobAgent, payload.Messages[1].ToAgent, "inbound row snapshots the recipient's agent_id")

	// The folder identity itself also carries the agent_id, so the sidebar
	// label for the selected folder leads with the agt_ handle too.
	assert.Equal(t, bobAgent, payload.AgentID, "the agent folder identity snapshots its stable agent_id")
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

// findMsg returns the message with the given subject from a folder page, or
// fails the test — a small helper for the mark-read scenarios.
func findMsg(t *testing.T, msgs []mailboxMsg, subject string) mailboxMsg {
	t.Helper()
	for _, m := range msgs {
		if m.Subject == subject {
			return m
		}
	}
	t.Fatalf("no message with subject %q in folder", subject)
	return mailboxMsg{}
}

// markRead POSTs to /api/mailbox/mark-read and returns the marked count.
func markRead(t *testing.T, dash http.Handler, body map[string]any) int {
	t.Helper()
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/mailbox/mark-read", body))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		Marked int `json:"marked"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	return resp.Marked
}

// Scenario: POST /api/mailbox/mark-read {ids,read} flips a single row's
// read-state both ways, and the change is visible at the read surface in BOTH
// the recipient's and the sender's folder (it is one shared row). This is the
// operator marking a stuck agent's received message read on its behalf.
func TestDashboardMailboxMarkRead_TogglesByID(t *testing.T) {
	f := newFlow(t)
	dash := seedMailboxes(t, f)

	// alice→bob "hi bob" starts unread in bob's inbox.
	opener := findMsg(t, getMailbox(t, dash, mbBob), "hi bob")
	require.False(t, opener.Read, "alice's opener starts unread")

	// Mark it read on bob's behalf.
	assert.Equal(t, 1, markRead(t, dash, map[string]any{
		"ids": []int64{opener.ID}, "read": true}))
	assert.True(t, findMsg(t, getMailbox(t, dash, mbBob), "hi bob").Read,
		"now read in bob's (recipient) folder")
	assert.True(t, findMsg(t, getMailbox(t, dash, mbAlice), "hi bob").Read,
		"same shared row reads as read in alice's (sender) folder too")

	// Re-marking read is an idempotent no-op (count 0, state unchanged).
	assert.Equal(t, 0, markRead(t, dash, map[string]any{
		"ids": []int64{opener.ID}, "read": true}), "already-read row doesn't re-transition")

	// Mark it back to unread.
	assert.Equal(t, 1, markRead(t, dash, map[string]any{
		"ids": []int64{opener.ID}, "read": false}))
	assert.False(t, findMsg(t, getMailbox(t, dash, mbBob), "hi bob").Read,
		"back to unread in bob's folder")
}

// Scenario: the ids path is DIRECTION-AGNOSTIC — marking a row the folder
// agent SENT (its read_at belongs to the OTHER party) flips it just the same.
// This locks in the deliberate asymmetry with the conv path (received-only,
// see TestDashboardMailboxMarkRead_MarkAllForConv): the per-message reader
// toggle and the bulk-selection path can reach an outbound row, by design.
func TestDashboardMailboxMarkRead_ByIDFlipsSentRow(t *testing.T) {
	f := newFlow(t)
	dash := seedMailboxes(t, f)

	// bob→alice "re: hi bob" is read in the seed; from bob's folder it is an
	// outbound row (bob sent it; to_conv = alice).
	sent := findMsg(t, getMailbox(t, dash, mbBob), "re: hi bob")
	require.Equal(t, "out", sent.Direction, "the row bob sent")
	require.True(t, sent.Read, "starts read")

	// Mark that SENT row unread by id — the ids path doesn't gate on direction.
	assert.Equal(t, 1, markRead(t, dash, map[string]any{
		"ids": []int64{sent.ID}, "read": false}))
	assert.False(t, findMsg(t, getMailbox(t, dash, mbBob), "re: hi bob").Read,
		"sent row now unread in bob's (sender) folder")
	assert.False(t, findMsg(t, getMailbox(t, dash, mbAlice), "re: hi bob").Read,
		"and in alice's (recipient) folder — same shared row")
}

// Scenario: POST /api/mailbox/mark-read {conv,read:true} marks every message
// the conv has RECEIVED read, and does NOT touch rows it SENT (read_at there
// belongs to the other party). The per-folder "mark all read" for a stuck
// agent.
func TestDashboardMailboxMarkRead_MarkAllForConv(t *testing.T) {
	f := newFlow(t)
	dash := seedMailboxes(t, f)

	// Add an unread message bob SENT (bob→alice) so we can prove the sent
	// side is left alone. Seed already has bob receiving one unread (hi bob)
	// and having sent one read (re: hi bob).
	g, err := db.GetAgentGroupByName("team")
	require.NoError(t, err)
	_, err = db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g.ID, FromConv: mbBob, ToConv: mbAlice,
		Subject: "bob outbound unread", Body: "x", CreatedAt: time.Now(),
	})
	require.NoError(t, err)

	// Mark all of bob's RECEIVED messages read — only "hi bob" qualifies.
	assert.Equal(t, 1, markRead(t, dash, map[string]any{
		"conv": mbBob, "read": true}), "only the one received-unread row flips")

	bob := getMailbox(t, dash, mbBob)
	assert.True(t, findMsg(t, bob, "hi bob").Read, "received message now read")
	assert.False(t, findMsg(t, bob, "bob outbound unread").Read,
		"bob's SENT row is untouched — its read-state belongs to alice")

	// Idempotent: a second mark-all flips nothing.
	assert.Equal(t, 0, markRead(t, dash, map[string]any{
		"conv": mbBob, "read": true}))
}

// Scenario: conv mode supports read=true only — marking a whole inbox unread
// is a footgun the endpoint refuses.
func TestDashboardMailboxMarkRead_ConvUnreadRejected(t *testing.T) {
	f := newFlow(t)
	dash := seedMailboxes(t, f)
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/mailbox/mark-read", map[string]any{"conv": mbBob, "read": false}))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}

// Scenario: a body naming neither ids nor conv is a 400 — the client must say
// what to mark.
func TestDashboardMailboxMarkRead_RequiresTarget(t *testing.T) {
	f := newFlow(t)
	dash := seedMailboxes(t, f)
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/mailbox/mark-read", map[string]any{"read": true}))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}

// Scenario: mark-read refuses an uncookied request, same human-consent gate
// as the delete/wipe mutations.
func TestDashboardMailboxMarkRead_RequiresAuth(t *testing.T) {
	newFlow(t)
	mux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(mux)
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost,
		"/api/mailbox/mark-read", map[string]any{"ids": []int64{1}, "read": true}))
	assert.Equal(t, http.StatusForbidden, rec.Code, "uncookied mark-read refused")
}

// --- pagination + server-side search --------------------------------

// mailboxPageResp mirrors the paginated /api/mailbox wire shape: a page
// of messages plus the pager metadata.
type mailboxPageResp struct {
	ID              string       `json:"id"`
	Kind            string       `json:"kind"`
	Title           string       `json:"title"`
	Messages        []mailboxMsg `json:"messages"`
	Page            int          `json:"page"`
	PageSize        int          `json:"page_size"`
	Total           int          `json:"total"`
	TotalUnfiltered int          `json:"total_unfiltered"`
}

// getMailboxPage fetches one page of a folder through the production
// handler, with optional search (q="" omits it) and pagination params
// (<=0 omits them, letting the server default).
func getMailboxPage(t *testing.T, dash http.Handler, id, q string, page, pageSize int) mailboxPageResp {
	t.Helper()
	params := url.Values{}
	params.Set("id", id)
	if q != "" {
		params.Set("q", q)
	}
	if page > 0 {
		params.Set("page", strconv.Itoa(page))
	}
	if pageSize > 0 {
		params.Set("page_size", strconv.Itoa(pageSize))
	}
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet,
		"/api/mailbox?"+params.Encode(), nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var p mailboxPageResp
	testharness.DecodeJSON(t, rec, &p)
	return p
}

// seedManyAgentMessages stands up alice + bob and inserts n alice→bob
// messages with deterministic subjects (msg-0000 … msg-(n-1)) and
// ascending timestamps, so the id order (the newest-first sort key)
// matches insertion order: msg-(n-1) is newest. Returns the dash handler.
func seedManyAgentMessages(t *testing.T, f *testharness.Flow, n int) http.Handler {
	t.Helper()
	g := f.HaveGroup("team")
	f.HaveMember("team", mbAlice)
	f.HaveMember("team", mbBob)
	f.HaveConvWithTitle(mbAlice, "alice")
	f.HaveConvWithTitle(mbBob, "bob")
	base := time.Now().Add(-time.Duration(n+1) * time.Minute)
	for i := 0; i < n; i++ {
		_, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID: g.ID, FromConv: mbAlice, ToConv: mbBob,
			Subject:   fmt.Sprintf("msg-%04d", i),
			Body:      fmt.Sprintf("body for message %d", i),
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
		require.NoError(t, err)
	}
	return dashHandlerForTest(t)
}

// subjectsOf collects the subjects of a page in order.
func subjectsOf(msgs []mailboxMsg) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Subject
	}
	return out
}

// Scenario: an agent folder pages newest-first. page_size=5 over 12
// messages yields pages of 5/5/2 covering every message once, with total
// + total_unfiltered constant across pages and the served page echoed.
func TestDashboardMailbox_PaginatesAgentFolder(t *testing.T) {
	f := newFlow(t)
	dash := seedManyAgentMessages(t, f, 12)

	seen := map[string]int{}
	wantSizes := []int{5, 5, 2}
	var firstNewest string
	for pg := 1; pg <= 3; pg++ {
		p := getMailboxPage(t, dash, mbBob, "", pg, 5)
		assert.Equal(t, pg, p.Page, "served page echoed")
		assert.Equal(t, 5, p.PageSize)
		assert.Equal(t, 12, p.Total, "total spans the whole folder")
		assert.Equal(t, 12, p.TotalUnfiltered)
		require.Len(t, p.Messages, wantSizes[pg-1], "page %d size", pg)
		if pg == 1 {
			firstNewest = p.Messages[0].Subject
		}
		for _, s := range subjectsOf(p.Messages) {
			seen[s]++
		}
	}
	assert.Equal(t, "msg-0011", firstNewest, "newest (highest id) leads page 1")
	assert.Len(t, seen, 12, "every message appears exactly once across the pages")
	for s, n := range seen {
		assert.Equal(t, 1, n, "no overlap for %s", s)
	}
}

// Scenario: a page past the last is pulled back to the last page (a stale
// "page 99" after deletions still lands on real rows), and the response
// reports the page actually served.
func TestDashboardMailbox_PageClampedPastLast(t *testing.T) {
	f := newFlow(t)
	dash := seedManyAgentMessages(t, f, 7)

	p := getMailboxPage(t, dash, mbBob, "", 99, 5)
	assert.Equal(t, 2, p.Page, "clamped to the last page (7 msgs / 5 = 2 pages)")
	assert.Equal(t, 7, p.Total)
	require.Len(t, p.Messages, 2, "the last page holds the remaining 2")
}

// Scenario: server-side search filters the WHOLE folder before paging —
// total reflects matches, total_unfiltered the folder, and only matching
// rows come back.
func TestDashboardMailbox_SearchFiltersWholeFolder(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("team")
	f.HaveMember("team", mbAlice)
	f.HaveMember("team", mbBob)
	f.HaveConvWithTitle(mbAlice, "alice")
	f.HaveConvWithTitle(mbBob, "bob")
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 10; i++ {
		_, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID: g.ID, FromConv: mbAlice, ToConv: mbBob,
			Subject: fmt.Sprintf("keep-%d", i), Body: "ordinary",
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
		require.NoError(t, err)
	}
	for i := 0; i < 3; i++ {
		_, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID: g.ID, FromConv: mbAlice, ToConv: mbBob,
			Subject: fmt.Sprintf("FINDME-%d", i), Body: "special",
			CreatedAt: base.Add(time.Duration(20+i) * time.Minute),
		})
		require.NoError(t, err)
	}
	dash := dashHandlerForTest(t)

	// Case-insensitive subject match, default page big enough for all 3.
	p := getMailboxPage(t, dash, mbBob, "findme", 0, 0)
	assert.Equal(t, 3, p.Total, "three subjects match the search")
	assert.Equal(t, 13, p.TotalUnfiltered, "folder still holds all 13")
	require.Len(t, p.Messages, 3)
	for _, m := range p.Messages {
		assert.Contains(t, m.Subject, "FINDME")
	}

	// Body match works too.
	pb := getMailboxPage(t, dash, mbBob, "special", 0, 0)
	assert.Equal(t, 3, pb.Total, "three bodies match the search")
}

// Scenario: search + pagination compose — q narrows the folder, then the
// page slices the matches.
func TestDashboardMailbox_SearchThenPaginate(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("team")
	f.HaveMember("team", mbAlice)
	f.HaveMember("team", mbBob)
	f.HaveConvWithTitle(mbAlice, "alice")
	f.HaveConvWithTitle(mbBob, "bob")
	base := time.Now().Add(-time.Hour)
	// 4 of 9 carry the needle.
	for i := 0; i < 9; i++ {
		subj := fmt.Sprintf("plain-%d", i)
		if i%2 == 0 && i < 8 {
			subj = fmt.Sprintf("needle-%d", i)
		}
		_, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID: g.ID, FromConv: mbAlice, ToConv: mbBob,
			Subject: subj, Body: "x", CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
		require.NoError(t, err)
	}
	dash := dashHandlerForTest(t)

	p1 := getMailboxPage(t, dash, "all", "needle", 1, 3)
	assert.Equal(t, 4, p1.Total, "four needles match")
	require.Len(t, p1.Messages, 3, "page 1 holds 3 of the 4 matches")
	for _, m := range p1.Messages {
		assert.Contains(t, m.Subject, "needle")
	}
	p2 := getMailboxPage(t, dash, "all", "needle", 2, 3)
	require.Len(t, p2.Messages, 1, "page 2 holds the last match")
	assert.Contains(t, p2.Messages[0].Subject, "needle")
}

// Scenario: search matches a counterpart's resolved title even though the
// title is not a column on agent_messages — the handler resolves which
// convs match and folds them into the query. A search for "carol" returns
// only carol's message, not the alice↔bob traffic.
func TestDashboardMailbox_SearchMatchesCounterpartTitle(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("team")
	f.HaveMember("team", mbAlice)
	f.HaveMember("team", mbBob)
	f.HaveMember("team", mbCarol)
	f.HaveConvWithTitle(mbAlice, "alice")
	f.HaveConvWithTitle(mbBob, "bob")
	f.HaveConvWithTitle(mbCarol, "carol")
	base := time.Now().Add(-time.Hour)
	// Two alice↔bob messages, one carol→bob message.
	_, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g.ID, FromConv: mbAlice, ToConv: mbBob,
		Subject: "ab one", Body: "x", CreatedAt: base,
	})
	require.NoError(t, err)
	_, err = db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g.ID, FromConv: mbBob, ToConv: mbAlice,
		Subject: "ab two", Body: "x", CreatedAt: base.Add(time.Minute),
	})
	require.NoError(t, err)
	_, err = db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g.ID, FromConv: mbCarol, ToConv: mbBob,
		Subject: "from carol", Body: "x", CreatedAt: base.Add(2 * time.Minute),
	})
	require.NoError(t, err)
	dash := dashHandlerForTest(t)

	p := getMailboxPage(t, dash, "all", "carol", 0, 0)
	assert.Equal(t, 3, p.TotalUnfiltered, "folder holds all three")
	require.Equal(t, 1, p.Total, "only the carol message matches the title search")
	require.Len(t, p.Messages, 1)
	assert.Equal(t, "from carol", p.Messages[0].Subject)
}

// Scenario: a freshly-started Codex agent can be registered before its
// conv_index title exists. The Groups tab names it from agents.pending_name;
// the Messages tab must use the same fallback for the folder title, row
// titles, and title search instead of showing only the stable agent id.
func TestDashboardMailbox_PendingNameTitlesUnindexedAgent(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("team")
	const codexConv = "mbox-code-1111-2222-333333333306"
	f.HaveMember("team", mbAlice)
	f.HaveMember("team", codexConv)
	f.HaveConvWithTitle(mbAlice, "alice")
	f.HavePendingName(codexConv, "codex-worker")
	_, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g.ID, FromConv: mbAlice, ToConv: codexConv,
		Subject: "startup", Body: "brief", CreatedAt: time.Now(),
	})
	require.NoError(t, err)
	dash := dashHandlerForTest(t)

	folder := getMailboxPage(t, dash, codexConv, "", 0, 0)
	assert.Equal(t, "codex-worker", folder.Title, "agent folder title uses the pending-name fallback")
	require.Len(t, folder.Messages, 1)
	assert.Equal(t, "codex-worker", folder.Messages[0].ToTitle,
		"message row recipient title uses the pending-name fallback")

	search := getMailboxPage(t, dash, "all", "codex-worker", 0, 0)
	require.Equal(t, 1, search.Total, "title search includes pending-name matches")
	require.Len(t, search.Messages, 1)
	assert.Equal(t, "startup", search.Messages[0].Subject)
	assert.Equal(t, "codex-worker", search.Messages[0].ToTitle)
}

// Scenario: the human folder paginates + searches in Go over its
// snapshot, with the same page/total contract as the agent folders.
func TestDashboardMailbox_HumanFolderPaginatesAndSearches(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")
	f.HaveMember("team", mbAlice)
	f.HaveConvWithTitle(mbAlice, "alice")
	for i := 0; i < 6; i++ {
		body := fmt.Sprintf("note %d", i)
		if i >= 4 {
			body = fmt.Sprintf("urgent note %d", i)
		}
		_, err := db.InsertHumanMessage(&db.HumanMessage{
			FromConv: mbAlice, FromTitle: "alice", Body: body,
		})
		require.NoError(t, err)
	}
	dash := dashHandlerForTest(t)

	p1 := getMailboxPage(t, dash, "human", "", 1, 4)
	assert.Equal(t, 6, p1.Total)
	assert.Equal(t, 6, p1.TotalUnfiltered)
	require.Len(t, p1.Messages, 4, "first human page")
	p2 := getMailboxPage(t, dash, "human", "", 2, 4)
	require.Len(t, p2.Messages, 2, "second human page")

	ps := getMailboxPage(t, dash, "human", "urgent", 0, 0)
	assert.Equal(t, 2, ps.Total, "two human notes match 'urgent'")
	assert.Equal(t, 6, ps.TotalUnfiltered)
	require.Len(t, ps.Messages, 2)
}

// Scenario: page_size is clamped to maxMailboxPageSize so a hand-crafted
// query can't ask the daemon for an unbounded page.
func TestDashboardMailbox_PageSizeClamped(t *testing.T) {
	f := newFlow(t)
	dash := seedManyAgentMessages(t, f, 3)
	p := getMailboxPage(t, dash, mbBob, "", 1, 100000)
	assert.LessOrEqual(t, p.PageSize, 500, "page_size clamped to the server cap")
	assert.Equal(t, 3, p.Total)
}

// Scenario: the dashboard's bulk delete splits a large selection into
// many small batched /api/mailbox/delete calls rather than one giant
// request (so the operator can watch a progress bar fill). Seed more
// messages than one batch, then delete them the way mail.js does — in
// sequential chunks — and assert each call reports its own count, the
// counts sum to the whole, and the folder ends empty. A re-delete of an
// already-removed chunk is a harmless no-op (count 0), matching the
// idempotent retry the batching relies on if a later batch were to fail.
func TestDashboardMailboxDelete_BatchedSequentialCalls(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("bulk")
	f.HaveMember("bulk", mbAlice)
	f.HaveMember("bulk", mbBob)
	f.HaveConvWithTitle(mbAlice, "alice")
	f.HaveConvWithTitle(mbBob, "bob")
	dash := dashHandlerForTest(t)

	const total = 120 // > one DELETE_BATCH (50) of mail.js ⇒ 3 batches
	base := time.Now().Add(-time.Hour)
	for i := range total {
		_, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID: g.ID, FromConv: mbAlice, ToConv: mbBob,
			Subject: "bulk", Body: "n",
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		})
		require.NoError(t, err)
	}

	// Collect every id from the firehose, then delete in chunks of 50 —
	// the same batch size mail.js uses. The mailbox read is paginated, so
	// pull a single page large enough to hold them all (page_size is capped
	// at maxMailboxPageSize=500 server-side, and total < that).
	all := getMailboxPage(t, dash, "all", "", 1, 500).Messages
	require.Len(t, all, total)
	ids := make([]int64, len(all))
	for i, m := range all {
		ids[i] = m.ID
	}

	const batch = 50
	deletedTotal := 0
	for i := 0; i < len(ids); i += batch {
		end := min(i+batch, len(ids))
		rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
			"/api/mailbox/delete", map[string]any{"ids": ids[i:end]}))
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var resp struct {
			Deleted int `json:"deleted"`
		}
		testharness.DecodeJSON(t, rec, &resp)
		assert.Equal(t, end-i, resp.Deleted, "each batch deletes exactly its chunk")
		deletedTotal += resp.Deleted
	}
	assert.Equal(t, total, deletedTotal, "batches sum to the whole selection")
	assert.Empty(t, getMailbox(t, dash, "all"), "firehose empty after the batched delete")

	// Idempotent retry: re-deleting the first chunk now removes nothing.
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodPost,
		"/api/mailbox/delete", map[string]any{"ids": ids[:batch]}))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		Deleted int `json:"deleted"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	assert.Equal(t, 0, resp.Deleted, "already-deleted ids delete nothing on retry")
}

// --- retired-agent filtering --------------------------------------------
//
// The Messages tab hides retired agents from the sidebar — and drops their
// traffic from the "all" firehose — unless the operator ticks "show retired
// agents" (the include_retired param). These tests pin both halves through
// the real handlers, plus the escape hatches: opening a retired folder
// directly still works, and the roster's "all" badge tracks the same scope
// the firehose serves.

// seedRetiredMailboxes stands up two active agents (alice, bob) and one
// retired agent (carol), with three messages: alice→bob (both active),
// alice→carol and carol→alice (each touches the retired agent). So one row
// survives the default filter and two are hidden.
func seedRetiredMailboxes(t *testing.T, f *testharness.Flow) http.Handler {
	t.Helper()
	g := f.HaveGroup("team")
	f.HaveMember("team", mbAlice)
	f.HaveMember("team", mbBob)
	f.HaveConvWithTitle(mbAlice, "alice")
	f.HaveConvWithTitle(mbBob, "bob")
	f.HaveConvWithTitle(mbCarol, "carol")
	f.HaveRetiredAgent(mbCarol)

	base := time.Now().Add(-time.Hour)
	mustMsg := func(from, to, subj string, at time.Time) {
		_, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID: g.ID, FromConv: from, ToConv: to,
			Subject: subj, Body: subj, CreatedAt: at,
		})
		require.NoError(t, err)
	}
	mustMsg(mbAlice, mbBob, "active only", base)
	mustMsg(mbAlice, mbCarol, "to retired", base.Add(time.Minute))
	mustMsg(mbCarol, mbAlice, "from retired", base.Add(2*time.Minute))

	return dashHandlerForTest(t)
}

// getMailboxesOpt fetches the roster, optionally opting into retired agents.
func getMailboxesOpt(t *testing.T, dash http.Handler, includeRetired bool) []mailboxEntry {
	t.Helper()
	url := "/api/mailboxes"
	if includeRetired {
		url += "?include_retired=1"
	}
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, url, nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var payload struct {
		Mailboxes []mailboxEntry `json:"mailboxes"`
	}
	testharness.DecodeJSON(t, rec, &payload)
	return payload.Mailboxes
}

// getMailboxPageRetired fetches one folder page, optionally opting into
// retired agents. Twin of getMailboxPage but for the include_retired flag.
func getMailboxPageRetired(t *testing.T, dash http.Handler, id string, includeRetired bool) mailboxPageResp {
	t.Helper()
	params := url.Values{}
	params.Set("id", id)
	if includeRetired {
		params.Set("include_retired", "1")
	}
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet,
		"/api/mailbox?"+params.Encode(), nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var p mailboxPageResp
	testharness.DecodeJSON(t, rec, &p)
	return p
}

// Scenario: by default the roster omits a retired agent's folder, keeps the
// active agents, and the "all" badge counts only non-retired traffic.
func TestDashboardMailboxes_HidesRetiredAgentsByDefault(t *testing.T) {
	f := newFlow(t)
	dash := seedRetiredMailboxes(t, f)

	boxes := getMailboxesOpt(t, dash, false)
	assert.Nil(t, findMailbox(boxes, mbCarol), "retired agent's folder is hidden by default")
	require.NotNil(t, findMailbox(boxes, mbAlice), "active agent stays listed")
	require.NotNil(t, findMailbox(boxes, mbBob), "active agent stays listed")

	all := findMailbox(boxes, "all")
	require.NotNil(t, all)
	assert.Equal(t, 1, all.Total, "the all badge counts only the active↔active row")
}

// Scenario: with include_retired the retired folder appears, flagged
// Retired, and the "all" badge counts every row again.
func TestDashboardMailboxes_ShowsRetiredAgentsWhenOptedIn(t *testing.T) {
	f := newFlow(t)
	dash := seedRetiredMailboxes(t, f)

	boxes := getMailboxesOpt(t, dash, true)
	carol := findMailbox(boxes, mbCarol)
	require.NotNil(t, carol, "retired folder appears with include_retired")
	assert.True(t, carol.Retired, "and is flagged retired")
	assert.Equal(t, "carol", carol.Title)

	alice := findMailbox(boxes, mbAlice)
	require.NotNil(t, alice)
	assert.False(t, alice.Retired, "an active agent is never flagged retired")

	all := findMailbox(boxes, "all")
	require.NotNil(t, all)
	assert.Equal(t, 3, all.Total, "the all badge counts every row when retired are shown")
}

// Scenario: the "all" firehose drops rows touching a retired agent by
// default (page + total), and serves the full set with include_retired.
func TestDashboardMailbox_AllFirehoseExcludesRetiredByDefault(t *testing.T) {
	f := newFlow(t)
	dash := seedRetiredMailboxes(t, f)

	p := getMailboxPageRetired(t, dash, "all", false)
	assert.Equal(t, 1, p.Total, "only the active↔active row is in scope")
	require.Len(t, p.Messages, 1)
	assert.Equal(t, "active only", p.Messages[0].Subject)

	p = getMailboxPageRetired(t, dash, "all", true)
	assert.Equal(t, 3, p.Total, "every row when opted in")
	assert.Len(t, p.Messages, 3)
}

// Scenario: opening a retired agent's folder directly still returns all of
// its mail — the exclude is firehose-only, so the operator who ticked "show
// retired" and clicked the folder sees everything in it.
func TestDashboardMailbox_RetiredFolderReadableDirectly(t *testing.T) {
	f := newFlow(t)
	dash := seedRetiredMailboxes(t, f)

	p := getMailboxPageRetired(t, dash, mbCarol, false)
	assert.Equal(t, 2, p.Total, "carol's two messages (to + from) regardless of include_retired")
	assert.Len(t, p.Messages, 2)
}

// seedHandoffMailboxes models a reincarnation: carol (the retired
// predecessor) handed off to bob (the live successor), and dave (an even
// older, also-retired incarnation) had earlier handed off to carol. Rows:
//
//	alice→bob   "active only"   active↔active                    — shown
//	carol→bob   <handoff>       retired predecessor → live succ  — shown (carve-out)
//	carol→bob   "stale chatter" retired → live, NOT a handoff    — hidden
//	dave→carol  <handoff>       retired → retired (both dead)    — hidden (recipient retired)
//
// So the default firehose holds two of the four rows: the active pair and
// the handoff that birthed the live agent.
func seedHandoffMailboxes(t *testing.T, f *testharness.Flow) http.Handler {
	t.Helper()
	g := f.HaveGroup("team")
	f.HaveMember("team", mbAlice)
	f.HaveMember("team", mbBob)
	f.HaveConvWithTitle(mbAlice, "alice")
	f.HaveConvWithTitle(mbBob, "bob")
	f.HaveConvWithTitle(mbCarol, "carol")
	f.HaveConvWithTitle(mbDave, "dave")
	f.HaveRetiredAgent(mbCarol)
	f.HaveRetiredAgent(mbDave)

	base := time.Now().Add(-time.Hour)
	mustMsg := func(from, to, subj string, at time.Time) {
		_, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID: g.ID, FromConv: from, ToConv: to,
			Subject: subj, Body: subj, CreatedAt: at,
		})
		require.NoError(t, err)
	}
	mustMsg(mbAlice, mbBob, "active only", base)
	mustMsg(mbCarol, mbBob, db.ReincarnationHandoffSubject, base.Add(time.Minute))
	mustMsg(mbCarol, mbBob, "stale chatter", base.Add(2*time.Minute))
	mustMsg(mbDave, mbCarol, db.ReincarnationHandoffSubject, base.Add(3*time.Minute))

	return dashHandlerForTest(t)
}

// Scenario: a reincarnation handoff from a now-retired predecessor to its
// live successor stays in the "all" firehose by default — it is the
// successor's birth record, and the per-agent folder already shows it. The
// carve-out is narrow: a retired sender's NON-handoff DM still drops, and a
// handoff whose recipient has itself since retired (a fully-historical chain
// link) stays hidden. Opting into retired surfaces all four rows.
func TestDashboardMailbox_AllFirehoseKeepsReincarnationHandoff(t *testing.T) {
	f := newFlow(t)
	dash := seedHandoffMailboxes(t, f)

	p := getMailboxPageRetired(t, dash, "all", false)
	assert.Equal(t, 2, p.Total, "active row + the handoff to the live successor")
	subjects := subjectsOf(p.Messages)
	assert.Contains(t, subjects, db.ReincarnationHandoffSubject,
		"the handoff bridging the retired predecessor to the live successor shows")
	assert.Contains(t, subjects, "active only")
	assert.NotContains(t, subjects, "stale chatter",
		"a retired sender's ordinary DM still drops — the carve-out is handoff-only")

	// Opting into retired restores the full set, including the dave→carol
	// handoff between two dead incarnations.
	pAll := getMailboxPageRetired(t, dash, "all", true)
	assert.Equal(t, 4, pAll.Total, "every row when retired are shown")

	// The live successor's own folder is unchanged by the carve-out: it
	// shows everything addressed to it, handoff and stale chatter alike.
	bob := getMailbox(t, dash, mbBob)
	assert.Len(t, bob, 3, "bob sees the active row, the handoff, and the stale DM")
}

// Scenario: the same carve-out applies to a group folder, which shares the
// exclude path with the firehose. carol (a retired ex-member) handed off to
// bob (a live member), so the handoff is in the group's scope via bob; it
// must survive the group folder's retired-member exclude just as it does the
// firehose's, while carol's ordinary DM still drops.
func TestDashboardMailbox_GroupFolderKeepsReincarnationHandoff(t *testing.T) {
	f := newFlow(t)
	dash := seedHandoffMailboxes(t, f)

	p := getMailboxPageRetired(t, dash, "group:team", false)
	subjects := subjectsOf(p.Messages)
	assert.Contains(t, subjects, db.ReincarnationHandoffSubject,
		"the handoff to the live member survives the group folder's retired exclude")
	assert.Contains(t, subjects, "active only")
	assert.NotContains(t, subjects, "stale chatter",
		"the retired ex-member's ordinary DM still drops from the group folder")
}

// --- group folders ------------------------------------------------------
//
// A group folder ("group:<name>") is the "view this group's messages"
// view: every message touching a CURRENT member (sender or recipient),
// plus the group's own multicasts (group_id). It is the union of its
// members' folders + the group channel — the chosen "all member traffic"
// semantics. These tests pin the roster entry and the scope through the
// real handlers.

// indexOf returns the position of a mailbox id in the roster, or -1.
func indexOf(boxes []mailboxEntry, id string) int {
	for i := range boxes {
		if boxes[i].ID == id {
			return i
		}
	}
	return -1
}

// seedGroupMailbox stands up group "team" with current members alice + bob,
// an outsider carol, and a never-a-member dave, then inserts five messages
// spanning every scope edge:
//
//	alice→bob       member↔member            — in scope
//	bob→alice       member↔member            — in scope
//	alice→carol     member→outsider DM       — in scope (a member is a party)
//	carol→dave      outsider↔outsider DM     — OUT of scope
//	carol→team      multicast to the group   — in scope via group_id only
//	                (carol broadcast then left: a channel row whose sender
//	                is no longer a member, caught by group_id, not the
//	                member set)
//
// So the group scope holds four of the five rows.
func seedGroupMailbox(t *testing.T, f *testharness.Flow) (http.Handler, *db.AgentGroup) {
	t.Helper()
	g := f.HaveGroup("team")
	f.HaveMember("team", mbAlice)
	f.HaveMember("team", mbBob)
	f.HaveConvWithTitle(mbAlice, "alice")
	f.HaveConvWithTitle(mbBob, "bob")
	f.HaveConvWithTitle(mbCarol, "carol")
	f.HaveConvWithTitle(mbDave, "dave")

	base := time.Now().Add(-time.Hour)
	mustMsg := func(from, to, subj string, at time.Time) {
		_, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID: g.ID, FromConv: from, ToConv: to,
			Subject: subj, Body: subj, CreatedAt: at,
		})
		require.NoError(t, err)
	}
	mustMsg(mbAlice, mbBob, "ab member dm", base)
	mustMsg(mbBob, mbAlice, "ba member dm", base.Add(time.Minute))
	mustMsg(mbAlice, mbCarol, "member to outsider", base.Add(2*time.Minute))
	// carol→dave carries NO group_id so the outsider↔outsider row can't sneak
	// in via the group channel.
	_, err := db.InsertAgentMessage(&db.AgentMessage{
		FromConv: mbCarol, ToConv: mbDave,
		Subject: "outsider chatter", Body: "outsider chatter",
		CreatedAt: base.Add(3 * time.Minute),
	})
	require.NoError(t, err)
	// A multicast to the group (to_conv empty, group_id set) from a sender
	// who is NOT a current member — only the group_id branch puts it in scope.
	_, err = db.InsertAgentMessage(&db.AgentMessage{
		GroupID: g.ID, FromConv: mbCarol, ToConv: "",
		Subject: "team broadcast", Body: "team broadcast",
		CreatedAt: base.Add(4 * time.Minute),
	})
	require.NoError(t, err)

	return dashHandlerForTest(t), g
}

// Scenario: the roster lists one folder per group, between the pinned
// virtual folders and the per-agent folders, carrying the group's member
// count and its in-scope message total.
func TestDashboardMailboxes_ListsGroupFolders(t *testing.T) {
	f := newFlow(t)
	dash, _ := seedGroupMailbox(t, f)

	boxes := getMailboxes(t, dash)
	grp := findMailbox(boxes, "group:team")
	require.NotNil(t, grp, "the group gets a folder")
	assert.Equal(t, "group", grp.Kind)
	assert.Equal(t, "team", grp.Title)
	assert.Equal(t, 2, grp.Members, "alice + bob are the current members")
	assert.Equal(t, 4, grp.Total, "four of the five rows touch the group scope")

	// Ordering: group folders sit after the pinned human folder and before
	// the per-agent folders.
	assert.Greater(t, indexOf(boxes, "group:team"), indexOf(boxes, "human"),
		"group folders follow the pinned virtual folders")
	assert.Less(t, indexOf(boxes, "group:team"), indexOf(boxes, mbAlice),
		"group folders precede the per-agent folders")
}

// Scenario: a group folder carries its current members' conv-ids
// (member_convs) so the sidebar can nest the matching agent folders beneath
// the group row when it is expanded. The list is the current member set —
// alice + bob — and an agent / human / "all" folder carries none.
func TestDashboardMailboxes_GroupFolderCarriesMemberConvs(t *testing.T) {
	f := newFlow(t)
	dash, _ := seedGroupMailbox(t, f)

	boxes := getMailboxes(t, dash)
	grp := findMailbox(boxes, "group:team")
	require.NotNil(t, grp)
	assert.ElementsMatch(t, []string{mbAlice, mbBob}, grp.MemberConvs,
		"the group folder lists its current members' conv-ids")

	// The nesting payload is group-only — a per-agent folder never carries it.
	alice := findMailbox(boxes, mbAlice)
	require.NotNil(t, alice)
	assert.Empty(t, alice.MemberConvs, "an agent folder carries no member_convs")
}

// Scenario: GET /api/mailbox?id=group:team returns all member traffic plus
// the group channel — newest-first, every row from→to (direction "") like
// the "all" firehose — and EXCLUDES the outsider↔outsider DM.
func TestDashboardMailbox_GroupFolderShowsAllMemberTraffic(t *testing.T) {
	f := newFlow(t)
	dash, _ := seedGroupMailbox(t, f)

	p := getMailboxPage(t, dash, "group:team", "", 0, 0)
	assert.Equal(t, "group", p.Kind)
	assert.Equal(t, 4, p.Total)
	assert.Equal(t, 4, p.TotalUnfiltered)
	require.Len(t, p.Messages, 4)

	subjects := map[string]bool{}
	for _, m := range p.Messages {
		subjects[m.Subject] = true
		assert.Equal(t, "", m.Direction, "a group folder renders from→to, no per-self direction")
	}
	assert.True(t, subjects["ab member dm"], "member↔member DM in scope")
	assert.True(t, subjects["ba member dm"], "member↔member DM in scope")
	assert.True(t, subjects["member to outsider"], "a member's DM to an outsider is in scope")
	assert.True(t, subjects["team broadcast"], "the group channel multicast is in scope via group_id")
	assert.False(t, subjects["outsider chatter"], "outsider↔outsider DM is NOT in the group scope")

	// Newest-first: the broadcast (last inserted) leads.
	assert.Equal(t, "team broadcast", p.Messages[0].Subject)
}

// Scenario: a group folder still honours server-side search — q narrows the
// in-scope set, total tracks matches, total_unfiltered the whole scope.
func TestDashboardMailbox_GroupFolderSearches(t *testing.T) {
	f := newFlow(t)
	dash, _ := seedGroupMailbox(t, f)

	p := getMailboxPage(t, dash, "group:team", "broadcast", 0, 0)
	assert.Equal(t, 1, p.Total, "one in-scope row matches 'broadcast'")
	assert.Equal(t, 4, p.TotalUnfiltered, "the whole group scope is still four rows")
	require.Len(t, p.Messages, 1)
	assert.Equal(t, "team broadcast", p.Messages[0].Subject)
}

// Scenario: an unknown group id is a 404 — a renamed/deleted group folder
// the frontend snaps back to "all".
func TestDashboardMailbox_GroupFolderUnknownIs404(t *testing.T) {
	f := newFlow(t)
	dash, _ := seedGroupMailbox(t, f)

	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet,
		"/api/mailbox?id=group:nope", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code, "body=%s", rec.Body.String())
}

// Scenario: an archived (soft-deleted) group gets no roster folder — parity
// with the Groups tab, which hides archived groups from its default listing
// (so there is no "view messages" deep link to reach one). The folder is
// still openable by id directly (the handler resolves an archived group).
func TestDashboardMailboxes_ArchivedGroupHasNoFolder(t *testing.T) {
	f := newFlow(t)
	dash, _ := seedGroupMailbox(t, f)

	// Active: the folder is listed.
	require.NotNil(t, findMailbox(getMailboxes(t, dash), "group:team"),
		"an active group has a folder")

	// Archive it → the roster drops the folder, but a direct read still works.
	require.NoError(t, db.ArchiveAgentGroup("team"))
	assert.Nil(t, findMailbox(getMailboxes(t, dash), "group:team"),
		"an archived group is dropped from the roster")
	p := getMailboxPage(t, dash, "group:team", "", 0, 0)
	assert.Equal(t, 4, p.Total, "but the archived group's folder is still readable by id")
}

// --- group folders + retired filtering ----------------------------------
//
// A group folder hides retired members' traffic by default — exactly the
// exclude the "all" firehose applies — and reveals it with include_retired.
// These tests pin both the folder page and the roster badge through the
// real handlers.

// seedGroupMailboxRetired stands up group "team" with current members
// alice + bob and a retired carol (never a member), then four messages
// spanning the retired-exclude edges:
//
//	alice→bob     member↔member          — in scope, neither retired   → kept
//	bob→team      member's channel post  — in scope via group_id       → kept
//	alice→carol   member→retired DM      — in scope (alice is member)   → hidden
//	carol→team    retired sender's post  — in scope via group_id        → hidden
//
// So the scope holds two rows by default and all four with include_retired.
// The two hidden rows mirror the all-firehose exclude: a member's DM to a
// retired agent drops on the recipient side, a channel multicast drops only
// when its own sender is retired (the member's broadcast survives).
func seedGroupMailboxRetired(t *testing.T, f *testharness.Flow) (http.Handler, *db.AgentGroup) {
	t.Helper()
	g := f.HaveGroup("team")
	f.HaveMember("team", mbAlice)
	f.HaveMember("team", mbBob)
	f.HaveConvWithTitle(mbAlice, "alice")
	f.HaveConvWithTitle(mbBob, "bob")
	f.HaveConvWithTitle(mbCarol, "carol")
	f.HaveRetiredAgent(mbCarol)

	base := time.Now().Add(-time.Hour)
	mustMsg := func(from, to, subj string, at time.Time) {
		_, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID: g.ID, FromConv: from, ToConv: to,
			Subject: subj, Body: subj, CreatedAt: at,
		})
		require.NoError(t, err)
	}
	mustMsg(mbAlice, mbBob, "active dm", base)
	mustMsg(mbBob, "", "member broadcast", base.Add(time.Minute))
	mustMsg(mbAlice, mbCarol, "member to retired", base.Add(2*time.Minute))
	mustMsg(mbCarol, "", "retired broadcast", base.Add(3*time.Minute))

	return dashHandlerForTest(t), g
}

// Scenario: a group folder drops rows touching a retired agent by default
// (page + total), and serves the full set with include_retired.
func TestDashboardMailbox_GroupFolderExcludesRetiredByDefault(t *testing.T) {
	f := newFlow(t)
	dash, _ := seedGroupMailboxRetired(t, f)

	p := getMailboxPageRetired(t, dash, "group:team", false)
	assert.Equal(t, "group", p.Kind)
	assert.Equal(t, 2, p.Total, "retired-touching rows are hidden by default")
	assert.Equal(t, 2, p.TotalUnfiltered)
	require.Len(t, p.Messages, 2)
	subjects := map[string]bool{}
	for _, m := range p.Messages {
		subjects[m.Subject] = true
	}
	assert.True(t, subjects["active dm"], "member↔member DM survives")
	assert.True(t, subjects["member broadcast"], "a current member's channel post survives")
	assert.False(t, subjects["member to retired"], "a member's DM to a retired agent is hidden")
	assert.False(t, subjects["retired broadcast"], "a retired sender's channel post is hidden")

	p = getMailboxPageRetired(t, dash, "group:team", true)
	assert.Equal(t, 4, p.Total, "every row when opted in")
	assert.Len(t, p.Messages, 4)
}

// Scenario: the roster's group badge tracks the same exclude scope the
// folder serves — two by default, four with include_retired — so the badge
// never disagrees with the folder it labels.
func TestDashboardMailboxes_GroupBadgeExcludesRetiredByDefault(t *testing.T) {
	f := newFlow(t)
	dash, _ := seedGroupMailboxRetired(t, f)

	grp := findMailbox(getMailboxesOpt(t, dash, false), "group:team")
	require.NotNil(t, grp)
	assert.Equal(t, 2, grp.Total, "the group badge counts only non-retired traffic by default")
	assert.Equal(t, 2, grp.Members, "alice + bob are the current members")

	grp = findMailbox(getMailboxesOpt(t, dash, true), "group:team")
	require.NotNil(t, grp)
	assert.Equal(t, 4, grp.Total, "the group badge counts every row with include_retired")
}

// Scenario: a retired ex-member nests back under its former group once the
// operator opts into retired agents. Retire unjoins the agent from every
// group, so its live membership row is gone — but its group_id-stamped
// messages persist, so buildGroupMailboxes reconstructs the nesting from
// message history. By default the retired ex-member is absent everywhere;
// with include_retired it reappears in member_convs (so the sidebar can nest
// it) AND in the flat agent list flagged Retired (the folder the nested row
// reuses). The Members count stays the current-member count throughout.
//
// Regression: before the fix member_convs was the live membership only, so a
// retired ex-member could never nest even with "show retired agents" on,
// despite its flat folder being shown right there in the Agents section.
func TestDashboardMailboxes_GroupNestsRetiredExMemberWhenOptedIn(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("team")
	f.HaveMember("team", mbAlice)
	f.HaveMember("team", mbBob)
	f.HaveMember("team", mbCarol)
	f.HaveConvWithTitle(mbAlice, "alice")
	f.HaveConvWithTitle(mbBob, "bob")
	f.HaveConvWithTitle(mbCarol, "carol")

	base := time.Now().Add(-time.Hour)
	mustMsg := func(from, to, subj string, at time.Time) {
		_, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID: g.ID, FromConv: from, ToConv: to,
			Subject: subj, Body: subj, CreatedAt: at,
		})
		require.NoError(t, err)
	}
	// Each member posts to the group channel — carol's post is the
	// group_id-stamped trace that survives her retirement.
	mustMsg(mbAlice, "", "alice broadcast", base)
	mustMsg(mbBob, "", "bob broadcast", base.Add(time.Minute))
	mustMsg(mbCarol, "", "carol broadcast", base.Add(2*time.Minute))

	// Retire carol the way production does — this unjoins her from "team",
	// leaving only her message history to tie her to the group.
	f.HaveRetiredAgent(mbCarol)
	dash := dashHandlerForTest(t)

	// Default: carol is gone from the group's nested members and the flat list.
	def := getMailboxesOpt(t, dash, false)
	grp := findMailbox(def, "group:team")
	require.NotNil(t, grp)
	assert.Equal(t, 2, grp.Members, "alice + bob are the current members")
	assert.ElementsMatch(t, []string{mbAlice, mbBob}, grp.MemberConvs,
		"a retired ex-member is absent from the nested list by default")
	assert.Nil(t, findMailbox(def, mbCarol),
		"the retired ex-member's flat folder is hidden by default")

	// Opted in: carol nests back under the group (recovered from her group
	// post) and shows in the flat list flagged retired.
	boxes := getMailboxesOpt(t, dash, true)
	grp = findMailbox(boxes, "group:team")
	require.NotNil(t, grp)
	assert.Equal(t, 2, grp.Members,
		"Members stays the current count — the retired ex-member is not a member")
	assert.ElementsMatch(t, []string{mbAlice, mbBob, mbCarol}, grp.MemberConvs,
		"the retired ex-member nests back under its former group when opted in")
	carol := findMailbox(boxes, mbCarol)
	require.NotNil(t, carol, "the nested folder reuses the flat agent entry")
	assert.True(t, carol.Retired, "and it is flagged retired")
}

// --- empty-mailbox filtering --------------------------------------------
//
// The Messages tab hides agents whose mailbox is empty — never sent or
// received any mail — from the sidebar unless the operator ticks "show
// agents without messages" (the include_empty param). Unlike retired this
// is a roster-only filter: an empty mailbox owns no agent_messages rows, so
// it touches neither the "all" firehose nor its badge. These tests pin the
// default-hide, the opt-in, the firehose-is-unaffected invariant, the
// sent-only boundary (received nothing ≠ empty mailbox), and that the empty
// and retired filters compose independently.

// fetchRoster GETs /api/mailboxes with an arbitrary opt-in query string and
// decodes the roster. Twin of getMailboxes for the multi-flag cases.
func fetchRoster(t *testing.T, dash http.Handler, query string) []mailboxEntry {
	t.Helper()
	u := "/api/mailboxes"
	if query != "" {
		u += "?" + query
	}
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, u, nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var payload struct {
		Mailboxes []mailboxEntry `json:"mailboxes"`
	}
	testharness.DecodeJSON(t, rec, &payload)
	return payload.Mailboxes
}

// getMailboxesEmptyOpt fetches the roster, optionally opting into empty
// agents (the include_empty param).
func getMailboxesEmptyOpt(t *testing.T, dash http.Handler, includeEmpty bool) []mailboxEntry {
	t.Helper()
	q := ""
	if includeEmpty {
		q = "include_empty=1"
	}
	return fetchRoster(t, dash, q)
}

// seedEmptyMailboxes stands up three agents with mail (alice↔bob both
// directions; dave sent-only) plus one empty agent (no mail at all). So the
// empty filter hides exactly one folder, while the sent-only agent — which
// received nothing but has a non-empty mailbox — stays visible.
func seedEmptyMailboxes(t *testing.T, f *testharness.Flow) http.Handler {
	t.Helper()
	g := f.HaveGroup("team")
	for _, c := range []string{mbAlice, mbBob, mbDave, mbEmpty} {
		f.HaveMember("team", c)
	}
	f.HaveConvWithTitle(mbAlice, "alice")
	f.HaveConvWithTitle(mbBob, "bob")
	f.HaveConvWithTitle(mbDave, "dave")
	f.HaveConvWithTitle(mbEmpty, "empty")

	base := time.Now().Add(-time.Hour)
	mustMsg := func(from, to, subj string, at time.Time) {
		_, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID: g.ID, FromConv: from, ToConv: to,
			Subject: subj, Body: subj, CreatedAt: at,
		})
		require.NoError(t, err)
	}
	mustMsg(mbAlice, mbBob, "hi bob", base)
	mustMsg(mbBob, mbAlice, "re: hi bob", base.Add(time.Minute))
	// dave only ever sends — received nothing, but the folder is non-empty.
	mustMsg(mbDave, mbAlice, "from dave", base.Add(2*time.Minute))

	return dashHandlerForTest(t)
}

// Scenario: by default the roster omits an agent whose mailbox is empty,
// keeps every agent that has mail — including a sent-only agent (received
// nothing, but its folder isn't empty) — and the "all" badge is unchanged
// (an empty mailbox owns no rows).
func TestDashboardMailboxes_HidesEmptyAgentsByDefault(t *testing.T) {
	f := newFlow(t)
	dash := seedEmptyMailboxes(t, f)

	boxes := getMailboxesEmptyOpt(t, dash, false)
	assert.Nil(t, findMailbox(boxes, mbEmpty), "empty-mailbox agent is hidden by default")
	require.NotNil(t, findMailbox(boxes, mbAlice), "an agent with mail stays listed")
	require.NotNil(t, findMailbox(boxes, mbBob), "an agent with mail stays listed")

	dave := findMailbox(boxes, mbDave)
	require.NotNil(t, dave, "a sent-only agent has a non-empty mailbox, so it stays listed")
	assert.Equal(t, 0, dave.In, "dave received nothing")
	assert.Equal(t, 1, dave.Out)
	assert.Equal(t, 1, dave.Total)

	all := findMailbox(boxes, "all")
	require.NotNil(t, all)
	assert.Equal(t, 3, all.Total, "the empty filter is roster-only — the firehose badge counts every row")
}

// Scenario: with include_empty the empty-mailbox agent reappears, with zero
// counts, and the firehose badge is still unchanged (it added no rows).
func TestDashboardMailboxes_ShowsEmptyAgentsWhenOptedIn(t *testing.T) {
	f := newFlow(t)
	dash := seedEmptyMailboxes(t, f)

	boxes := getMailboxesEmptyOpt(t, dash, true)
	empty := findMailbox(boxes, mbEmpty)
	require.NotNil(t, empty, "empty folder appears with include_empty")
	assert.Equal(t, 0, empty.Total, "and carries zero counts")
	assert.Equal(t, "empty", empty.Title)

	all := findMailbox(boxes, "all")
	require.NotNil(t, all)
	assert.Equal(t, 3, all.Total, "the badge is unchanged — the empty agent adds no rows")
}

// Scenario: an empty agent's folder is still readable directly (the filter
// is roster-only), returning an empty page rather than 404 — opening it is
// an explicit ask, same escape hatch a retired folder gets.
func TestDashboardMailbox_EmptyFolderReadableDirectly(t *testing.T) {
	f := newFlow(t)
	dash := seedEmptyMailboxes(t, f)

	p := getMailboxPage(t, dash, mbEmpty, "", 0, 0)
	assert.Equal(t, 0, p.Total, "an empty folder opens to zero messages")
	assert.Empty(t, p.Messages)
}

// Scenario: the empty and retired filters compose independently. A retired
// agent (with mail) and an empty active agent are both hidden by default;
// each reappears only under its own opt-in, and neither flag implies the
// other. (Disjoint in practice: retired ⇒ has the mail that put it in the
// roster; empty ⇒ active-only.)
func TestDashboardMailboxes_EmptyAndRetiredFiltersCompose(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("team")
	f.HaveMember("team", mbAlice)
	f.HaveMember("team", mbBob)
	f.HaveMember("team", mbEmpty)
	f.HaveConvWithTitle(mbAlice, "alice")
	f.HaveConvWithTitle(mbBob, "bob")
	f.HaveConvWithTitle(mbCarol, "carol")
	f.HaveConvWithTitle(mbEmpty, "empty")
	f.HaveRetiredAgent(mbCarol)

	base := time.Now().Add(-time.Hour)
	mustMsg := func(from, to, subj string, at time.Time) {
		_, err := db.InsertAgentMessage(&db.AgentMessage{
			GroupID: g.ID, FromConv: from, ToConv: to,
			Subject: subj, Body: subj, CreatedAt: at,
		})
		require.NoError(t, err)
	}
	mustMsg(mbAlice, mbBob, "active", base)
	mustMsg(mbAlice, mbCarol, "to retired", base.Add(time.Minute))

	dash := dashHandlerForTest(t)

	// Default: both the retired (carol) and empty (mbEmpty) folders are gone.
	boxes := getMailboxes(t, dash)
	assert.Nil(t, findMailbox(boxes, mbCarol), "retired hidden by default")
	assert.Nil(t, findMailbox(boxes, mbEmpty), "empty hidden by default")
	require.NotNil(t, findMailbox(boxes, mbAlice), "an active agent with mail stays listed")

	// include_empty alone brings the empty folder, not the retired one.
	boxes = getMailboxesEmptyOpt(t, dash, true)
	require.NotNil(t, findMailbox(boxes, mbEmpty), "empty shown with include_empty")
	assert.Nil(t, findMailbox(boxes, mbCarol), "retired still hidden — include_empty doesn't imply it")

	// include_retired alone brings the retired folder, not the empty one.
	boxes = getMailboxesOpt(t, dash, true)
	require.NotNil(t, findMailbox(boxes, mbCarol), "retired shown with include_retired")
	assert.Nil(t, findMailbox(boxes, mbEmpty), "empty still hidden — include_retired doesn't imply it")

	// Both opt-ins: every folder is present.
	boxes = fetchRoster(t, dash, "include_retired=1&include_empty=1")
	require.NotNil(t, findMailbox(boxes, mbCarol), "retired shown with both opt-ins")
	require.NotNil(t, findMailbox(boxes, mbEmpty), "empty shown with both opt-ins")
}
