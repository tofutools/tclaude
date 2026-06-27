package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TestWhoami_ReportsStableAgentID pins JOH-27 PR2 on the real daemon
// surface: /v1/whoami carries the stable agent_id (alongside conv_id) so
// `tclaude agent whoami` can lead with it instead of the rotating conv-id.
func TestWhoami_ReportsStableAgentID(t *testing.T) {
	f := newFlow(t)
	const convID = "33333333-3333-3333-3333-333333333333"
	f.HaveGroup("g")
	f.HaveMember("g", convID) // enrolls the agent → mints an agent_id

	want, err := db.AgentIDForConv(convID)
	require.NoError(t, err)
	require.NotEmpty(t, want, "enrolled agent must have an agent_id")

	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/whoami", nil), convID)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "GET /v1/whoami body=%s", rec.Body.String())

	var resp struct {
		AgentID string `json:"agent_id"`
		ConvID  string `json:"conv_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode body=%s", rec.Body.String())
	assert.Equal(t, want, resp.AgentID, "whoami must report the stable agent_id")
	assert.Equal(t, convID, resp.ConvID, "conv_id still present for back-compat")
}

// TestLookup_ReturnsStableAgentID: /v1/lookup resolves a selector and
// returns the target's stable agent_id (the field the CLI now prints),
// without dropping the conv_id the daemon-internal callers still read.
func TestLookup_ReturnsStableAgentID(t *testing.T) {
	f := newFlow(t)
	const convID = "44444444-4444-4444-4444-444444444444"
	f.HaveGroup("g")
	f.HaveMember("g", convID)

	want, err := db.AgentIDForConv(convID)
	require.NoError(t, err)
	require.NotEmpty(t, want)

	r := agentd.AsAgentPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/lookup?selector="+convID, nil), convID)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "GET /v1/lookup body=%s", rec.Body.String())

	var resp struct {
		ConvID  string `json:"conv_id"`
		AgentID string `json:"agent_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode body=%s", rec.Body.String())
	assert.Equal(t, want, resp.AgentID, "lookup must include the stable agent_id")
	assert.Equal(t, convID, resp.ConvID, "conv_id still emitted")
}
