package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type dismissFlow struct {
	t   *testing.T
	f   *testharness.Flow
	mux http.Handler
}

func newDismissFlow(t *testing.T, agents ...string) *dismissFlow {
	f := newFlow(t)
	f.HaveGroup("design")
	for i, conv := range agents {
		f.HaveMember("design", conv)
		f.HaveAliveSession(conv, "hdis"+string(rune('a'+i)), "tclaude-hdis"+string(rune('a'+i)), f.TestCwd("work"))
		require.NoError(t, db.GrantAgentPermission(conv, agentd.PermHumanNotify, "test"))
	}
	return &dismissFlow{t: t, f: f, mux: dashMessageMux(t)}
}

func (d *dismissFlow) ask(conv, subject string) int64 {
	d.t.Helper()
	rec := testharness.Serve(d.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(d.t, http.MethodPost, "/v1/notify-human", map[string]any{
		"subject": subject, "body": "Pick one.", "options": []string{"A", "B"},
	}), conv))
	require.Equal(d.t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		ID int64 `json:"id"`
	}
	require.NoError(d.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.ID
}

func (d *dismissFlow) operatorDismiss(body map[string]any) *httptest.ResponseRecorder {
	return testharness.Serve(d.mux, testharness.JSONRequest(d.t, http.MethodPost, "/api/human-messages/dismiss", body))
}

func (d *dismissFlow) agentDismiss(conv string, body map[string]any) *httptest.ResponseRecorder {
	return testharness.Serve(d.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(d.t, http.MethodPost, "/v1/notify-human/dismiss", body), conv))
}

// openDecisionIDs reads the snapshot the companion's "to decide" queue is built
// from, keeping what the dashboard counts as open.
func (d *dismissFlow) openDecisionIDs() []int64 {
	d.t.Helper()
	snap := testharness.Serve(d.mux, testharness.JSONRequest(d.t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(d.t, http.StatusOK, snap.Code)
	var s struct {
		Messages []struct {
			ID          int64    `json:"id"`
			Options     []string `json:"options"`
			Decision    string   `json:"decision"`
			DismissedAt string   `json:"dismissed_at"`
		} `json:"messages"`
	}
	require.NoError(d.t, json.Unmarshal(snap.Body.Bytes(), &s))
	ids := []int64{}
	for _, m := range s.Messages {
		if len(m.Options) > 1 && m.Decision == "" && m.DismissedAt == "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

func inboxSubjects(t *testing.T, conv string) []string {
	t.Helper()
	rows, err := db.ListAgentMessagesForConv(conv, 100)
	require.NoError(t, err)
	out := []string{}
	for _, r := range rows {
		out = append(out, r.Subject)
	}
	return out
}

// The operator dismisses decisions one at a time or all at once: they leave
// the queue, stay in Messages marked dismissed, and each asking agent gets one
// message naming its dismissed decisions, not one per decision.
func TestDecisionDismiss_OperatorDismissesAndAgentsAreTold(t *testing.T) {
	const a, b = "hdis-aaaa-bbbb-cccc-000000000001", "hdis-aaaa-bbbb-cccc-000000000002"
	d := newDismissFlow(t, a, b)
	a1, a2, b1, a3 := d.ask(a, "Layout?"), d.ask(a, "Colour?"), d.ask(b, "Name?"), d.ask(a, "Font?")
	require.ElementsMatch(t, []int64{a1, a2, b1, a3}, d.openDecisionIDs())

	rec := d.operatorDismiss(map[string]any{"ids": []int64{a3}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"Decision #" + itoa64(a3) + " dismissed by the operator"}, inboxSubjects(t, a))

	rec = d.operatorDismiss(map[string]any{"ids": []int64{a1, a2, b1}, "reason": "plan changed"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "3", pick(t, rec.Body.Bytes(), "dismissed"))
	assert.Empty(t, d.openDecisionIDs())
	assert.Len(t, inboxSubjects(t, a), 2, "one more message for agent a, for both its decisions")
	assert.Contains(t, inboxSubjects(t, a), "2 decisions dismissed by the operator")
	assert.Equal(t, []string{"Decision #" + itoa64(b1) + " dismissed by the operator"}, inboxSubjects(t, b))
	rows, err := db.ListAgentMessagesForConv(a, 100)
	require.NoError(t, err)
	for _, r := range rows {
		if r.Subject == "2 decisions dismissed by the operator" {
			assert.Contains(t, r.Body, "Reason: plan changed")
			assert.Contains(t, r.Body, "#"+itoa64(a1)+": Layout?")
		}
	}

	m, err := db.GetHumanMessage(a1)
	require.NoError(t, err)
	assert.Equal(t, db.HumanDismissedByOperator, m.DismissedBy)
	assert.Equal(t, "plan changed", m.DismissReason)
	assert.Empty(t, m.Decision)

	// First one wins: a dismissed decision cannot be answered, nor dismissed again.
	reply := postDashReply(t, d.mux, map[string]any{"id": a1, "choice": 1})
	assert.Equal(t, http.StatusConflict, reply.Code, reply.Body.String())
	assert.Equal(t, http.StatusConflict, d.operatorDismiss(map[string]any{"ids": []int64{a1}}).Code)
	assert.Equal(t, http.StatusBadRequest, d.operatorDismiss(map[string]any{"ids": []int64{}}).Code)
}

// An answered decision cannot be dismissed afterwards.
func TestDecisionDismiss_AnswerFirstWins(t *testing.T) {
	const a = "hdis-aaaa-bbbb-cccc-000000000003"
	d := newDismissFlow(t, a)
	id := d.ask(a, "Layout?")
	require.Equal(t, http.StatusOK, postDashReply(t, d.mux, map[string]any{"id": id, "choice": 2}).Code)
	rec := d.operatorDismiss(map[string]any{"ids": []int64{id}})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusConflict, d.agentDismiss(a, map[string]any{"ids": []int64{id}}).Code)
}

// An agent dismisses only decisions it asked, by id or all at once, and is
// not sent a message about its own dismissal.
func TestDecisionDismiss_AgentDismissesOnlyItsOwn(t *testing.T) {
	const a, b = "hdis-aaaa-bbbb-cccc-000000000004", "hdis-aaaa-bbbb-cccc-000000000005"
	d := newDismissFlow(t, a, b)
	a1, _, b1 := d.ask(a, "Layout?"), d.ask(a, "Colour?"), d.ask(b, "Name?")

	rec := d.agentDismiss(a, map[string]any{"ids": []int64{b1}})
	assert.Equal(t, http.StatusNotFound, rec.Code, "another agent's decision reads as not found: %s", rec.Body.String())
	assert.Equal(t, http.StatusBadRequest, d.agentDismiss(a, map[string]any{"ids": []int64{a1}, "all": true}).Code)
	assert.Equal(t, http.StatusBadRequest, d.agentDismiss(a, map[string]any{}).Code)

	rec = d.agentDismiss(a, map[string]any{"ids": []int64{a1}, "reason": "decided myself"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	m, err := db.GetHumanMessage(a1)
	require.NoError(t, err)
	assert.Equal(t, db.HumanDismissedByAgent, m.DismissedBy)
	assert.Equal(t, "decided myself", m.DismissReason)

	rec = d.agentDismiss(a, map[string]any{"all": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "1", pick(t, rec.Body.Bytes(), "dismissed"), "only a2 was still open")
	assert.Equal(t, []int64{b1}, d.openDecisionIDs(), "agent b's decision is untouched")
	assert.Empty(t, inboxSubjects(t, a), "the agent knows; no nudge for its own dismissal")

	rec = d.agentDismiss(a, map[string]any{"all": true})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "0", pick(t, rec.Body.Bytes(), "dismissed"))

	// A human caller has no agent identity on the agent route.
	human := testharness.Serve(d.f.Mux, testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human/dismiss", map[string]any{"all": true}))
	assert.NotEqual(t, http.StatusOK, human.Code)
}

func pick(t *testing.T, raw []byte, key string) string {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &m))
	return string(m[key])
}
