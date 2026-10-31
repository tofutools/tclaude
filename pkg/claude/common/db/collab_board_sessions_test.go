package db

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func boardSessionAgents(t *testing.T) []string {
	t.Helper()
	sessions, err := ListCollabBoardSessions()
	require.NoError(t, err)
	out := []string{}
	for _, s := range sessions {
		out = append(out, s.AgentID)
	}
	return out
}

// Every path that sets retired_at ends the sessions, not only the ones a
// daemon handler goes through, and unretiring does not bring them back.
func TestCollabBoardSessionsEndOnEveryRetirePath(t *testing.T) {
	setupTestDB(t)
	_, err := CreateCollabBoard("plan", "Plan", "operator", 10)
	require.NoError(t, err)
	d, err := Open()
	require.NoError(t, err)
	for name, retire := range map[string]func(agentID, conv string) error{
		"by id":   func(id, _ string) error { _, err := RetireAgentByID(id, "test", "gone"); return err },
		"by conv": func(_, conv string) error { _, err := RetireAgent(conv, "test", "gone"); return err },
		"raw update": func(id, _ string) error {
			_, err := d.Exec(`UPDATE agents SET retired_at = ? WHERE agent_id = ?`, 1, id)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			conv := "conv-" + name
			agentID, _, err := EnsureAgentForConv(conv, "test")
			require.NoError(t, err)
			_, err = OpenCollabBoardSession("plan", agentID)
			require.NoError(t, err)
			require.Equal(t, []string{agentID}, boardSessionAgents(t))

			require.NoError(t, retire(agentID, conv))
			assert.Empty(t, boardSessionAgents(t))

			_, err = d.Exec(`UPDATE agents SET retired_at = NULL WHERE agent_id = ?`, agentID)
			require.NoError(t, err)
			assert.Empty(t, boardSessionAgents(t))
		})
	}
}

// An edit that lands after its agent retired applies, but cannot leave a
// session the retire already ended: nothing would ever end that one.
func TestCollabBoardSessionsARetiredAgentGetsNone(t *testing.T) {
	setupTestDB(t)
	_, err := CreateCollabBoard("plan", "Plan", "operator", 10)
	require.NoError(t, err)
	agentID, _, err := EnsureAgentForConv("conv", "test")
	require.NoError(t, err)
	_, err = RetireAgentByID(agentID, "test", "gone")
	require.NoError(t, err)

	_, accepted, err := ApplyCollabBoardEdit("plan", agentID, agentID,
		[]CollabBoardElement{{ID: "e", Version: 1, Body: json.RawMessage(`{}`)}}, nil, 100, 100)
	require.NoError(t, err)
	require.Equal(t, 1, accepted)
	assert.Empty(t, boardSessionAgents(t))

	_, err = OpenCollabBoardSession("plan", agentID)
	require.ErrorIs(t, err, ErrCollabBoardAgentRetired)
}

func TestCollabBoardSessionCloseErrors(t *testing.T) {
	setupTestDB(t)
	_, err := CreateCollabBoard("plan", "Plan", "operator", 10)
	require.NoError(t, err)
	require.ErrorIs(t, CloseCollabBoardSession("plan", "agt_x"), ErrCollabBoardNoSession)
	require.ErrorIs(t, CloseCollabBoardSession("nope", "agt_x"), ErrCollabBoardNotFound)
}
