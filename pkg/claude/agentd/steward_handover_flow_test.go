package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const (
	handoverWorker = "hand-aaaa-bbbb-cccc-000000000001"
	handoverOther  = "hand-aaaa-bbbb-cccc-000000000002"
)

type stewardHandoverFlow struct {
	*stewardCarryFlow
}

func newStewardHandoverFlow(t *testing.T) *stewardHandoverFlow {
	c := newStewardCarryFlow(t)
	setSteward(t, "", "")
	c.f.HaveGroup("team")
	for conv, tmux := range map[string]string{handoverWorker: "hand1", handoverOther: "hand2"} {
		c.f.HaveConvWithTitle(conv, "worker-"+tmux)
		c.f.HaveMember("team", conv)
		c.f.HaveAliveSession(conv, tmux, "tclaude-"+tmux, c.f.TestCwd(tmux))
	}
	return &stewardHandoverFlow{c}
}

func (s *stewardHandoverFlow) handover(conv, then string) *httptest.ResponseRecorder {
	s.t.Helper()
	return s.post("/api/steward/handover", map[string]any{"agent": s.agentID(conv), "then": then})
}

func (s *stewardHandoverFlow) mustHandover(conv, then string) {
	s.t.Helper()
	rec := s.handover(conv, then)
	require.Equalf(s.t, http.StatusOK, rec.Code, "handover body=%s", rec.Body.String())
}

type handoverPreview struct {
	Gains []string `json:"gains"`
	Extra struct {
		Standing []string `json:"standing"`
	} `json:"extra"`
	Denied   []string `json:"denied"`
	Withheld []string `json:"withheld"`
	Refused  string   `json:"refused"`
	Previous *struct {
		Name  string   `json:"name"`
		Loses []string `json:"loses"`
		Keeps []string `json:"keeps"`
	} `json:"previous"`
}

func (s *stewardHandoverFlow) preview(conv string) handoverPreview {
	s.t.Helper()
	rec := testharness.Serve(s.dash, testharness.JSONRequest(s.t, http.MethodGet, "/api/steward/handover?agent="+s.agentID(conv), nil))
	require.Equalf(s.t, http.StatusOK, rec.Code, "preview body=%s", rec.Body.String())
	var out handoverPreview
	require.NoError(s.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func (s *stewardHandoverFlow) roster() map[string]string {
	s.t.Helper()
	rec := testharness.Serve(s.f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(s.t, http.MethodGet, "/v1/steward", nil)))
	require.Equalf(s.t, http.StatusOK, rec.Code, "roster body=%s", rec.Body.String())
	var out struct {
		Roster []struct {
			AgentID string `json:"agent_id"`
			Seat    string `json:"seat"`
		} `json:"roster"`
	}
	require.NoError(s.t, json.Unmarshal(rec.Body.Bytes(), &out))
	seats := map[string]string{}
	for _, row := range out.Roster {
		seats[row.AgentID] = row.Seat
	}
	return seats
}

// addressee sends to "steward" as the operator and says who got it.
func (s *stewardHandoverFlow) addressee(convs ...string) string {
	s.t.Helper()
	rec := s.post("/api/operator-message", map[string]any{"to": "steward", "body": "who is it"})
	require.Equalf(s.t, http.StatusAccepted, rec.Code, "send body=%s", rec.Body.String())
	for _, conv := range convs {
		rows, err := db.ListAgentMessagesForConv(conv, 1)
		require.NoError(s.t, err)
		if len(rows) > 0 && rows[0].Body == "who is it" {
			return conv
		}
	}
	return ""
}

func TestStewardHandover_ReplacesTheSteward(t *testing.T) {
	s := newStewardHandoverFlow(t)
	old := s.summon()
	require.NoError(t, db.SetAgentPermissionOverride(old, agentd.PermAgentRename, db.PermEffectGrant, "dashboard"))
	require.NoError(t, db.SetAgentPermissionOverride(handoverWorker, agentd.PermHostRead, db.PermEffectDeny, "dashboard"))
	require.NoError(t, db.SetAgentPermissionOverride(handoverWorker, agentd.PermAgentRename, db.PermEffectGrant, "dashboard"))

	preview := s.preview(handoverWorker)
	assert.Contains(t, preview.Gains, agentd.PermFleetRead)
	assert.Contains(t, preview.Denied, agentd.PermHostRead)
	assert.Contains(t, preview.Extra.Standing, agentd.PermAgentRename, "what it holds above a Steward's defaults is shown")
	require.NotNil(t, preview.Previous)
	assert.Contains(t, preview.Previous.Loses, agentd.PermFleetRead)
	assert.Contains(t, preview.Previous.Keeps, agentd.PermAgentRename)

	s.mustHandover(handoverWorker, "roster")

	assert.Equal(t, "grant", stewardOverride(t, handoverWorker, agentd.PermFleetRead))
	assert.Equal(t, "deny", stewardOverride(t, handoverWorker, agentd.PermHostRead), "a deny stands")
	members, err := db.ListGroupsForConv(handoverWorker)
	require.NoError(t, err)
	assert.Len(t, members, 2, "it keeps its own group")

	_, held := stewardOverrideHeld(t, old, agentd.PermFleetRead)
	assert.False(t, held, "the old Steward loses what the role gave it")
	assert.Equal(t, "grant", stewardOverride(t, old, agentd.PermAgentRename), "and keeps what the operator gave it")

	assert.Equal(t, map[string]string{s.agentID(handoverWorker): "active", s.agentID(old): "standby"}, s.roster())
	assert.Equal(t, handoverWorker, s.addressee(handoverWorker, old), "the address follows the seat")

	rows, err := db.ListAgentMessagesForConv(handoverWorker, 10)
	require.NoError(t, err)
	var subjects []string
	for _, row := range rows {
		subjects = append(subjects, row.Subject)
	}
	assert.Contains(t, subjects, "You are now the Steward")
}

func TestStewardHandover_RosterSwitchIsReversible(t *testing.T) {
	s := newStewardHandoverFlow(t)
	s.mustHandover(handoverWorker, "roster")
	require.NoError(t, db.GrantAgentPermission(handoverOther, agentd.PermAgentRename, "dashboard"))
	// The operator takes one slug off this Steward. Coming back must not restore it.
	_, err := db.RevokeAgentPermission(handoverWorker, agentd.PermBoardsWrite)
	require.NoError(t, err)

	s.mustHandover(handoverOther, "roster")
	_, held := stewardOverrideHeld(t, handoverWorker, agentd.PermFleetRead)
	require.False(t, held)
	assert.Equal(t, s.agentID(handoverOther), activeOf(s.roster()))

	assert.Contains(t, s.preview(handoverWorker).Withheld, agentd.PermBoardsWrite)
	s.mustHandover(handoverWorker, "keep")
	assert.Equal(t, "grant", stewardOverride(t, handoverWorker, agentd.PermFleetRead))
	_, held = stewardOverrideHeld(t, handoverWorker, agentd.PermBoardsWrite)
	assert.False(t, held, "what the operator took off stays off")

	assert.Equal(t, map[string]string{s.agentID(handoverWorker): "active"}, s.roster(), "keep leaves the roster")
	_, held = stewardOverrideHeld(t, handoverOther, agentd.PermFleetRead)
	assert.False(t, held)
	assert.Equal(t, "grant", stewardOverride(t, handoverOther, agentd.PermAgentRename), "its own grant outlives the role")
}

func activeOf(seats map[string]string) string {
	for agentID, seat := range seats {
		if seat == "active" {
			return agentID
		}
	}
	return ""
}

func TestStewardHandover_RetiringTheOldSteward(t *testing.T) {
	s := newStewardHandoverFlow(t)
	old := s.summon()
	oldAgent := s.agentID(old)
	s.mustHandover(handoverWorker, "retire")
	state, err := db.AgentState(old)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateRetired, state)
	assert.NotContains(t, s.roster(), oldAgent)
}

func TestStewardHandover_StepDownLeavesNobody(t *testing.T) {
	s := newStewardHandoverFlow(t)
	s.mustHandover(handoverWorker, "roster")
	rec := s.post("/api/steward/step-down", map[string]any{"then": "roster"})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, map[string]string{s.agentID(handoverWorker): "standby"}, s.roster())
	assert.Equal(t, http.StatusNotFound, s.post("/api/operator-message", map[string]any{"to": "steward", "body": "x"}).Code)

	rec = s.post("/api/steward/roster/remove", map[string]any{"agent": s.agentID(handoverWorker)})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Empty(t, s.roster())
}

// No route takes an agent, whatever it holds: there is no slug to grant or sudo.
func TestStewardHandover_IsHumanOnly(t *testing.T) {
	s := newStewardHandoverFlow(t)
	require.NoError(t, db.GrantAgentPermission(handoverWorker, agentd.PermPermissionsGrant, "test"))
	for _, route := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/steward", nil},
		{http.MethodGet, "/v1/steward/handover?agent=" + s.agentID(handoverWorker), nil},
		{http.MethodPost, "/v1/steward/handover", map[string]any{"agent": s.agentID(handoverWorker)}},
		{http.MethodPost, "/v1/steward/step-down", map[string]any{}},
		{http.MethodPost, "/v1/steward/roster/remove", map[string]any{"agent": s.agentID(handoverWorker)}},
	} {
		rec := testharness.Serve(s.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, route.method, route.path, route.body), handoverWorker))
		assert.Equalf(t, http.StatusForbidden, rec.Code, "%s %s body=%s", route.method, route.path, rec.Body.String())
	}
	assert.Empty(t, s.roster())
}

// A member description does not make a Steward, also not for an agent that used
// to be one.
func TestStewardHandover_DemotedAgentCannotRejoinByGroupEdit(t *testing.T) {
	s := newStewardHandoverFlow(t)
	s.mustHandover(handoverWorker, "roster")
	require.Equal(t, http.StatusOK, s.post("/api/steward/step-down", map[string]any{}).Code)

	g, err := db.GetAgentGroupByName("steward")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g.ID, ConvID: handoverWorker, Descr: "Reusable scribe scope: steward/"}))
	seats := s.roster()
	assert.Equal(t, "standby", seats[s.agentID(handoverWorker)])
	assert.Equal(t, http.StatusNotFound, s.post("/api/operator-message", map[string]any{"to": "steward", "body": "x"}).Code)
}

func TestStewardHandover_HiddenWithCollabOff(t *testing.T) {
	s := newStewardHandoverFlow(t)
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features.Collab = false
	require.NoError(t, config.Save(cfg))
	rec := s.handover(handoverWorker, "roster")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestStewardHandover_OperatorSummonReplacesAnOfflineSteward(t *testing.T) {
	s := newStewardHandoverFlow(t)
	s.mustHandover(handoverWorker, "roster")
	s.f.MarkOffline("tclaude-hand1")
	fresh := s.summon()
	assert.Equal(t, s.agentID(fresh), activeOf(s.roster()))
	assert.Equal(t, "standby", s.roster()[s.agentID(handoverWorker)])
	_, held := stewardOverrideHeld(t, handoverWorker, agentd.PermFleetRead)
	assert.False(t, held)
}

func TestSteward_SummonWithoutABriefGetsTheDaemons(t *testing.T) {
	s := newStewardHandoverFlow(t)
	rec := testharness.Serve(s.f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
		"name": "steward", "scope": map[string]any{"kind": "steward"},
	})))
	require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
	rows, err := db.ListAgentMessagesForConv(decodeScribeResp(t, rec).ConvID, 10)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	assert.Contains(t, rows[len(rows)-1].Body, "tclaude agent board show")
	assert.Contains(t, rows[len(rows)-1].Body, "--ask-human")
}

// A Steward that predates the record has an empty one, so what it was born with
// has to go by its label.
func TestStewardHandover_SummonedStewardLosesItsBirthGrants(t *testing.T) {
	s := newStewardHandoverFlow(t)
	require.NoError(t, db.SetStewardCarriedPermission(agentd.PermAgentRename, db.Grant(), "test"))
	old := s.summon()
	require.NoError(t, db.CloseStewardRecord(s.agentID(old), agentd.PermAgentRename))
	require.NoError(t, db.OfferStewardPermissions(s.agentID(old), db.StewardRecordOpened))

	s.mustHandover(handoverWorker, "keep")
	_, held := stewardOverrideHeld(t, old, agentd.PermAgentRename)
	assert.False(t, held)
}

// What a promoted agent held by its own birth, as another scribe, is not the role's.
func TestStewardHandover_KeepsAnEarlierScribesOwnGrants(t *testing.T) {
	s := newStewardHandoverFlow(t)
	require.NoError(t, db.SetAgentPermissionOverride(handoverWorker, agentd.PermFleetRead, db.PermEffectGrant, "<human>:scribe-summon:correlation-id=x"))
	s.mustHandover(handoverWorker, "roster")
	require.Equal(t, http.StatusOK, s.post("/api/steward/step-down", map[string]any{}).Code)
	assert.Equal(t, "grant", stewardOverride(t, handoverWorker, agentd.PermFleetRead))
}

// A slug the agent held through the role's time is not one the operator took off.
func TestStewardHandover_LaterRevokeIsNotWithheld(t *testing.T) {
	s := newStewardHandoverFlow(t)
	require.NoError(t, db.SetAgentPermissionOverride(handoverWorker, agentd.PermHostRead, db.PermEffectDeny, "dashboard"))
	s.mustHandover(handoverWorker, "roster")
	require.Equal(t, http.StatusOK, s.post("/api/steward/step-down", map[string]any{}).Code)
	_, err := db.RevokeAgentPermission(handoverWorker, agentd.PermHostRead)
	require.NoError(t, err)
	assert.NotContains(t, s.preview(handoverWorker).Withheld, agentd.PermHostRead)
}

func TestSteward_NameBelongsToTheSteward(t *testing.T) {
	s := newStewardHandoverFlow(t)
	rec := testharness.Serve(s.f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
		"name": "steward", "slugs": []string{agentd.PermFleetRead}, "brief": "Help.",
	})))
	assert.Equalf(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}
