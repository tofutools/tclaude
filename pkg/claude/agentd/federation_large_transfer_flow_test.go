package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestFederation_AgentTransferLimitRefusesBeforeSourceChanges(t *testing.T) {
	fh := newFedHarness(t)
	aid := fedMoveSource(t, fh)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/config", map[string]any{"agent_transfer_max_bytes": 64})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/move-agent", map[string]any{"agent": moveSourceConv, "peer": "bob", "group": "receiver"})
	require.Equal(t, 400, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "federation.agent_transfer_max_bytes")
	a, err := db.GetAgent(aid)
	require.NoError(t, err)
	require.True(t, a.Active())
	require.Equal(t, moveSourceConv, a.CurrentConvID)
	moves, err := db.ListFederationAgentMoves()
	require.NoError(t, err)
	require.Empty(t, moves)
	offers, err := db.ListFederationBundleOffers("out")
	require.NoError(t, err)
	require.Empty(t, offers)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/config", map[string]any{"agent_transfer_max_bytes": 0})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	fedStartMove(t, fh)
}
