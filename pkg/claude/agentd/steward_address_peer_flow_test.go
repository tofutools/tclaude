package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-046wcm: a group whose lead is named "steward". With no built-in Steward
// online the address "steward" reaches that lead, To and Cc alike, instead of
// failing "no Steward is online". With a built-in Steward running as well, the
// send is refused as ambiguous, naming both, rather than guessing; a running
// Steward that shares its name with no agent keeps the address, and the
// operator's own message always means the Steward.

const (
	stewardPeerWorker = "stwp-aaaa-bbbb-cccc-000000000001"
	stewardPeerLead   = "stwp-aaaa-bbbb-cccc-000000000002"
	stewardPeerOther  = "stwp-aaaa-bbbb-cccc-000000000003"
)

func newStewardPeerFlow(t *testing.T) *stewardCarryFlow {
	t.Helper()
	c := newStewardCarryFlow(t)
	setSteward(t, "", "")
	c.f.HaveGroup("crew")
	for conv, title := range map[string]string{stewardPeerWorker: "worker", stewardPeerLead: "steward", stewardPeerOther: "other"} {
		c.f.HaveConvWithTitle(conv, title)
		c.f.HaveMember("crew", conv)
		c.f.HaveAliveSession(conv, "lbl-"+title, "tclaude-stwp-"+title, c.f.TestCwd(title))
	}
	return c
}

func stewardPeerSend(t *testing.T, c *stewardCarryFlow, from string, req map[string]any) (int, string) {
	t.Helper()
	rec := testharness.Serve(c.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/messages", req), from))
	return rec.Code, rec.Body.String()
}

func TestStewardAddress_ALeadNamedStewardIsReachedWhenNoStewardRuns(t *testing.T) {
	c := newStewardPeerFlow(t)

	code, body := stewardPeerSend(t, c, stewardPeerWorker, map[string]any{"to": "steward", "body": "report to the lead"})
	require.Equal(t, http.StatusOK, code, body)
	assert.True(t, convMessagesContain(t, stewardPeerLead, "report to the lead"))

	code, body = stewardPeerSend(t, c, stewardPeerWorker, map[string]any{"to": stewardPeerOther, "cc": []string{"steward"}, "body": "copied to the lead"})
	require.Equal(t, http.StatusOK, code, body)
	assert.True(t, convMessagesContain(t, stewardPeerLead, "copied to the lead"))
}

func TestStewardAddress_AnEarlierStewardDoesNotClaimTheName(t *testing.T) {
	c := newStewardPeerFlow(t)
	earlier := c.summon()
	c.shutDown(earlier)

	code, body := stewardPeerSend(t, c, stewardPeerWorker, map[string]any{"to": "steward", "body": "after the Steward left"})

	require.Equal(t, http.StatusOK, code, body)
	assert.True(t, convMessagesContain(t, stewardPeerLead, "after the Steward left"))
	assert.False(t, convMessagesContain(t, earlier, "after the Steward left"))
}

func TestStewardAddress_ARunningStewardAndAPeerOfTheSameNameIsAmbiguous(t *testing.T) {
	c := newStewardPeerFlow(t)
	steward := c.summon()
	require.NoError(t, db.GrantAgentPermissionWithScope(stewardPeerWorker, agentd.PermMessageDirect, "", "test"))

	code, body := stewardPeerSend(t, c, stewardPeerWorker, map[string]any{"to": "steward", "body": "which one"})

	require.Equal(t, http.StatusConflict, code, body)
	assert.Contains(t, body, `"code":"ambiguous"`)
	assert.Contains(t, body, c.agentID(steward), "the error names the Steward")
	assert.Contains(t, body, c.agentID(stewardPeerLead), "and the agent of the same name")
	assert.False(t, convMessagesContain(t, steward, "which one"))
	assert.False(t, convMessagesContain(t, stewardPeerLead, "which one"))

	// The operator's own message means the Steward, whatever an agent is named.
	rec := c.post("/api/operator-message", map[string]any{"to": "steward", "body": "from the operator"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.True(t, convMessagesContain(t, steward, "from the operator"))
	assert.False(t, convMessagesContain(t, stewardPeerLead, "from the operator"))
}

func TestStewardAddress_ARunningStewardKeepsAnAddressNoAgentShares(t *testing.T) {
	c := newStewardCarryFlow(t)
	setSteward(t, "", "")
	c.f.HaveConvWithTitle(stewardPeerWorker, "worker")
	c.f.HaveAliveSession(stewardPeerWorker, "lbl-worker", "tclaude-stwp-worker", c.f.TestCwd("worker"))
	steward := c.summon()
	require.NoError(t, db.GrantAgentPermissionWithScope(stewardPeerWorker, agentd.PermMessageDirect, "", "test"))

	code, body := stewardPeerSend(t, c, stewardPeerWorker, map[string]any{"to": "steward", "body": "for the companion"})

	require.Equal(t, http.StatusOK, code, body)
	assert.True(t, convMessagesContain(t, steward, "for the companion"))
}

func TestStewardAddress_WithNoSuchAgentNoStewardIsStillTheAnswer(t *testing.T) {
	c := newStewardCarryFlow(t)
	setSteward(t, "", "")
	c.f.HaveConvWithTitle(stewardPeerWorker, "worker")
	c.f.HaveAliveSession(stewardPeerWorker, "lbl-worker", "tclaude-stwp-worker", c.f.TestCwd("worker"))
	earlier := c.summon()
	c.shutDown(earlier)

	code, body := stewardPeerSend(t, c, stewardPeerWorker, map[string]any{"to": "steward", "body": "nobody"})

	assert.Equal(t, http.StatusNotFound, code)
	assert.Contains(t, body, "no Steward is online; the operator can summon one from the companion window")
}
