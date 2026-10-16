package agentd_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type backgroundTerminalRow struct {
	Kind        string `json:"kind"`
	TmuxSession string `json:"tmux_session"`
	Key         string `json:"key"`
	Label       string `json:"label"`
	AgentID     string `json:"agent_id"`
	ConvID      string `json:"conv_id"`
	Which       string `json:"which"`
}

func fetchBackgroundTerminals(t *testing.T, mux http.Handler) []backgroundTerminalRow {
	t.Helper()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/background-terminals", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Terminals []backgroundTerminalRow `json:"terminals"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	return resp.Terminals
}

// Scenario: the Terminals tab lists tclaude tmux sessions nobody is attached
// to — an agent, a plain session, an agent's web shell, and an unrecognised
// session — each classified with the pane key its attach opens under. A
// session with a client attached (an open web terminal or native window) is
// not in the background and is left out.
func TestBackgroundTerminals_ListsUnattachedSessions(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const agentConv = "bgta-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(agentConv, "idle-worker")
	f.HaveAliveSession(agentConv, "spwn-bgta", "tmux-bgta", f.TestCwd("bgta"))
	f.HaveEnrolledAgent(agentConv)
	agentID, err := db.AgentIDForConv(agentConv)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)

	const plainConv = "bgtb-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(plainConv, "quick question")
	f.HaveAliveSession(plainConv, "spwn-bgtb", "tmux-bgtb", f.TestCwd("bgtb"))

	const watchedConv = "bgtc-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(watchedConv, "watched")
	f.HaveAliveSession(watchedConv, "spwn-bgtc", "tmux-bgtc", f.TestCwd("bgtc"))
	f.World.Tmux.AttachClient("/dev/pts/9", "tmux-bgtc")

	sum := sha256.Sum256([]byte(agentConv))
	shell := fmt.Sprintf("tclaude-term-%x-current", sum[:8])
	f.World.Tmux.MarkAlive(shell)
	f.World.Tmux.MarkAlive("someone-elses-session")

	rows := fetchBackgroundTerminals(t, mux)
	byTmux := map[string]backgroundTerminalRow{}
	for _, row := range rows {
		byTmux[row.TmuxSession] = row
	}
	assert.NotContains(t, byTmux, "tmux-bgtc", "an attached session is not in the background")

	require.Contains(t, byTmux, "tmux-bgta")
	assert.Equal(t, backgroundTerminalRow{
		Kind: "agent", TmuxSession: "tmux-bgta", Key: "window:" + agentID,
		Label: "idle-worker", AgentID: agentID, ConvID: agentConv,
	}, byTmux["tmux-bgta"])

	require.Contains(t, byTmux, "tmux-bgtb")
	assert.Equal(t, "session", byTmux["tmux-bgtb"].Kind)
	assert.Equal(t, "window:"+plainConv, byTmux["tmux-bgtb"].Key)

	require.Contains(t, byTmux, shell)
	assert.Equal(t, "shell", byTmux[shell].Kind)
	assert.Equal(t, "current", byTmux[shell].Which)
	assert.Equal(t, "term:"+agentID+":current:", byTmux[shell].Key)
	assert.Equal(t, "idle-worker", byTmux[shell].Label)

	require.Contains(t, byTmux, "someone-elses-session")
	assert.Equal(t, "other", byTmux["someone-elses-session"].Kind)
	assert.Empty(t, byTmux["someone-elses-session"].Key, "an unrecognised session has no dashboard attach")

	assert.Equal(t, "agent", rows[0].Kind, "agents sort first")
}
