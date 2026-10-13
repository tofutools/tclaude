package agentd_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These tests cover the server-paged sidebar roster: GET /api/mailboxes
// returns one newest-activity-first page of agent folders plus totals over
// the whole filtered list, filters agent folders by the sidebar text
// server-side, drops predecessor generations on request, embeds an
// expanded group's member folders, and always describes the open folder.

type rosterBox struct {
	ID          string      `json:"id"`
	Kind        string      `json:"kind"`
	Title       string      `json:"title"`
	PrevGen     bool        `json:"prev_gen"`
	Retired     bool        `json:"retired"`
	Members     int         `json:"members"`
	Unread      int         `json:"unread"`
	MemberBoxes  []rosterBox `json:"member_boxes"`
	MembersTotal int         `json:"members_total"`
}

type rosterResp struct {
	Mailboxes       []rosterBox `json:"mailboxes"`
	AgentsTotal     int         `json:"agents_total"`
	AgentsUnread    int         `json:"agents_unread"`
	Selected        string      `json:"selected"`
	SelectedValid   bool        `json:"selected_valid"`
	SelectedMailbox *rosterBox  `json:"selected_mailbox"`
}

func getRoster(t *testing.T, dash http.Handler, params url.Values) rosterResp {
	t.Helper()
	rec := testharness.Serve(dash, testharness.JSONRequest(t, http.MethodGet, "/api/mailboxes?"+params.Encode(), nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var out rosterResp
	testharness.DecodeJSON(t, rec, &out)
	return out
}

func (r rosterResp) agentIDs() []string {
	var ids []string
	for _, mb := range r.Mailboxes {
		if mb.Kind == "agent" {
			ids = append(ids, mb.ID)
		}
	}
	return ids
}

func boxIDs(boxes []rosterBox) []string {
	ids := make([]string, 0, len(boxes))
	for _, mb := range boxes {
		ids = append(ids, mb.ID)
	}
	return ids
}

func (r rosterResp) group(name string) *rosterBox {
	for i := range r.Mailboxes {
		if r.Mailboxes[i].Kind == "group" && r.Mailboxes[i].Title == name {
			return &r.Mailboxes[i]
		}
	}
	return nil
}

// seedRoster creates five agents in group "crew" (agent i titled
// "crew-<i>"), each having sent one unread message to agent 0 at minute i,
// so agent 4 has the newest activity. Returns their conv-ids.
func seedRoster(t *testing.T, f *testharness.Flow) []string {
	t.Helper()
	f.HaveGroup("crew")
	convs := make([]string, 5)
	for i := range convs {
		convs[i] = fmt.Sprintf("rost-%04d-1111-2222-333333333333", i)
		f.HaveMember("crew", convs[i])
		f.HaveConvWithTitle(convs[i], fmt.Sprintf("crew-%d", i))
	}
	base := time.Now().Add(-time.Hour)
	for i := 1; i < len(convs); i++ {
		_, err := db.InsertAgentMessage(&db.AgentMessage{FromConv: convs[i], ToConv: convs[0],
			Subject: "hi", CreatedAt: base.Add(time.Duration(i) * time.Minute)})
		require.NoError(t, err)
	}
	return convs
}

// Scenario: the flat agent list is a newest-activity-first page; the totals
// describe every folder, and agents_limit=0 returns totals only.
func TestDashboardMailboxes_PagesAgentFolders(t *testing.T) {
	f := newFlow(t)
	convs := seedRoster(t, f)
	dash := dashHandlerForTest(t)

	r := getRoster(t, dash, url.Values{"agents_limit": {"2"}})
	assert.Equal(t, []string{convs[0], convs[4]}, r.agentIDs(),
		"newest activity first: agent 4 sent the last message and agent 0 received it (same instant, conv-id tiebreak)")
	assert.Equal(t, 5, r.AgentsTotal)
	assert.Equal(t, 4, r.AgentsUnread, "agent 0's four unread received messages count, page or not")

	r = getRoster(t, dash, url.Values{"agents_limit": {"0"}})
	assert.Empty(t, r.agentIDs(), "a folded section asks for totals only")
	assert.Equal(t, 5, r.AgentsTotal)
	require.NotNil(t, r.group("crew"), "group rows are always present")
}

// Scenario: the sidebar filter matches agent folders server-side by display
// title, conv-id and group name.
func TestDashboardMailboxes_FiltersAgentFoldersServerSide(t *testing.T) {
	f := newFlow(t)
	convs := seedRoster(t, f)
	dash := dashHandlerForTest(t)

	r := getRoster(t, dash, url.Values{"q": {"CREW-3"}})
	assert.Equal(t, []string{convs[3]}, r.agentIDs(), "title match, case-insensitive")
	assert.Equal(t, 1, r.AgentsTotal)

	r = getRoster(t, dash, url.Values{"q": {"rost-0002"}})
	assert.Equal(t, []string{convs[2]}, r.agentIDs(), "conv-id fragment")

	r = getRoster(t, dash, url.Values{"q": {"crew"}})
	assert.Equal(t, 5, r.AgentsTotal, "group name (and titles) match every member")
}

// Scenario: hide_prev_gens drops a predecessor generation's folder (its mail
// stays); without it the folder is shown and flagged prev_gen.
func TestDashboardMailboxes_HidesPrevGensServerSide(t *testing.T) {
	f := newFlow(t)
	convs := seedRoster(t, f)
	const successor = "rost-succ-1111-2222-333333333333"
	_, err := db.RotateAgentConv(convs[1], successor, "reincarnate")
	require.NoError(t, err)
	dash := dashHandlerForTest(t)

	r := getRoster(t, dash, url.Values{})
	require.Contains(t, r.agentIDs(), convs[1])
	for _, mb := range r.Mailboxes {
		if mb.ID == convs[1] {
			assert.True(t, mb.PrevGen, "the predecessor folder is flagged")
		}
	}

	r = getRoster(t, dash, url.Values{"hide_prev_gens": {"1"}, "selected": {convs[1]}})
	assert.NotContains(t, r.agentIDs(), convs[1])
	assert.Equal(t, 4, r.AgentsTotal)
	assert.False(t, r.SelectedValid, "an open predecessor folder is no longer shown")
}

// Scenario: an expanded group carries its visible member folders inline;
// a collapsed one only its member count.
func TestDashboardMailboxes_EmbedsExpandedGroupMembers(t *testing.T) {
	f := newFlow(t)
	convs := seedRoster(t, f)
	dash := dashHandlerForTest(t)

	r := getRoster(t, dash, url.Values{"agents_limit": {"0"}})
	crew := r.group("crew")
	require.NotNil(t, crew)
	assert.Equal(t, 5, crew.Members)
	assert.Empty(t, crew.MemberBoxes, "collapsed: no member folders")

	r = getRoster(t, dash, url.Values{"agents_limit": {"0"}, "expand": {"crew"}, "q": {"crew-2"}})
	crew = r.group("crew")
	require.NotNil(t, crew)
	require.Len(t, crew.MemberBoxes, 1, "expanded members obey the filter")
	assert.Equal(t, convs[2], crew.MemberBoxes[0].ID)
	assert.Equal(t, "crew-2", crew.MemberBoxes[0].Title)
}

// Scenario: an expanded group's member folders are paged like the flat
// list — newest activity first, members_limit ("<n>:<group>") of them, with
// members_total counting all.
func TestDashboardMailboxes_PagesExpandedGroupMembers(t *testing.T) {
	f := newFlow(t)
	convs := seedRoster(t, f)
	dash := dashHandlerForTest(t)

	r := getRoster(t, dash, url.Values{"agents_limit": {"0"}, "expand": {"crew"}, "members_limit": {"2:crew"}})
	crew := r.group("crew")
	require.NotNil(t, crew)
	assert.Equal(t, []string{convs[0], convs[4]}, boxIDs(crew.MemberBoxes), "newest activity first")
	assert.Equal(t, 5, crew.MembersTotal)

	r = getRoster(t, dash, url.Values{"agents_limit": {"0"}, "expand": {"crew"}})
	assert.Len(t, r.group("crew").MemberBoxes, 5, "default page holds the whole small group")
}

// Scenario: the open agent folder is described even when it is off the
// page, and selected_valid follows the visibility toggles.
func TestDashboardMailboxes_DescribesSelectedFolder(t *testing.T) {
	f := newFlow(t)
	convs := seedRoster(t, f)
	dash := dashHandlerForTest(t)

	r := getRoster(t, dash, url.Values{"agents_limit": {"1"}, "selected": {convs[1]}})
	assert.NotContains(t, r.agentIDs(), convs[1], "off the one-folder page")
	assert.Equal(t, convs[1], r.Selected)
	assert.True(t, r.SelectedValid)
	require.NotNil(t, r.SelectedMailbox)
	assert.Equal(t, "crew-1", r.SelectedMailbox.Title)

	_, err := db.RetireAgent(convs[1], "test", "")
	require.NoError(t, err)
	r = getRoster(t, dash, url.Values{"selected": {convs[1]}})
	assert.False(t, r.SelectedValid, "retired agents are hidden by default")
	r = getRoster(t, dash, url.Values{"selected": {convs[1]}, "include_retired": {"1"}})
	assert.True(t, r.SelectedValid)
}
