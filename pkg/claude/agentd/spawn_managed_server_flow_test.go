package agentd_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// A server-authoritative spawn (OpenCode) readies its managed server before
// enrollment: the server's conversation id becomes the agent's, and a server
// that does not start refuses the spawn with nothing enrolled.
func TestSpawn_ManagedServerSetsTheConvOrRefuses(t *testing.T) {
	for _, tc := range []struct {
		name     string
		startErr error
	}{
		{name: "the server starts"},
		{name: "the server does not start", startErr: errors.New("port already in use")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("alpha")
			// An OpenCode spawn continues in the background past a short grace;
			// hold the answer inline so it carries the outcome. The grace is
			// only an upper bound: the spawn answers as soon as it settles.
			t.Cleanup(agentd.SetOpenCodeAsyncSpawnResponseGraceForTest(time.Minute))
			const serverConv = "ses_0123456789abcdefghijklmnop"
			var started atomic.Int32
			t.Cleanup(agentd.SetOpenCodeRuntimeForTest(func(sessionID, _, _, _ string) (agentd.OpenCodeRuntimeFixture, error) {
				started.Add(1)
				if tc.startErr != nil {
					return agentd.OpenCodeRuntimeFixture{}, tc.startErr
				}
				return agentd.OpenCodeRuntimeFixture{SessionID: sessionID, ConvID: serverConv,
					ServerURL: "http://127.0.0.1:1", Password: "pw", PID: 1}, nil
			}))

			spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "w", "harness": harness.OpenCodeName})
			assert.Equal(t, int32(1), started.Load(), "one server start per spawn")
			g, err := db.GetAgentGroupByName("alpha")
			require.NoError(t, err)
			members, err := db.ListAgentGroupMembers(g.ID)
			require.NoError(t, err)
			if tc.startErr == nil {
				require.Equal(t, http.StatusOK, spawn.Code, "body=%s", spawn.Raw)
				var resp struct {
					ConvID string `json:"conv_id"`
				}
				require.NoError(t, json.Unmarshal(spawn.Raw, &resp))
				assert.Equal(t, serverConv, resp.ConvID, "the server's conversation is the agent's")
				require.Len(t, members, 1)
				assert.Equal(t, serverConv, members[0].ConvID)
				return
			}
			require.Equal(t, http.StatusInternalServerError, spawn.Code, "body=%s", spawn.Raw)
			var got struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(spawn.Raw, &got))
			assert.Equal(t, "spawn", got.Code)
			assert.Equal(t, "failed to start managed OpenCode server: port already in use", got.Error)
			assert.Empty(t, members, "a refused spawn enrolls nobody")
		})
	}
}
