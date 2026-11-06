package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These pin reincarnation's refusals before anything is launched: each answers
// with its typed error, launches nothing, holds no launch claim, and adds no
// actor or session. (A past generation is refused too, as stale_generation,
// but the handler resolves a request to the agent's head first, so only a
// rotation racing the request reaches that check; it is not pinned here.)

// reincarnateLaunchCounter counts launches without starting anything.
type reincarnateLaunchCounter struct{ launches int }

func (s *reincarnateLaunchCounter) SpawnNew(clcommon.SpawnArgs) error    { s.launches++; return nil }
func (s *reincarnateLaunchCounter) SpawnResume(clcommon.SpawnArgs) error { s.launches++; return nil }

func TestReincarnatePreconditions_RefusalsLaunchNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		setup    func(t *testing.T, f *testharness.Flow) string
		status   int
		code     string
		errorHas string
	}{
		{"the target is offline",
			func(t *testing.T, f *testharness.Flow) string {
				spawn := spawnSourceThenEditItsSandbox(t, f)
				f.MarkOffline(spawn.TmuxSession)
				return spawn.ConvID
			},
			http.StatusServiceUnavailable, "no_tmux", "the agent is offline"},
		{"the target is a shell agent",
			func(t *testing.T, f *testharness.Flow) string {
				f.HaveGroup("crew")
				spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "sh", "harness": "shell", "command": "bash"})
				require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
				return spawn.ConvID
			},
			http.StatusUnprocessableEntity, "unsupported_harness", "shell agents cannot be reincarnated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			src := tc.setup(t, f)
			agentsBefore, err := db.ListActiveAgents()
			require.NoError(t, err)
			sessionsBefore, err := db.ListSessions()
			require.NoError(t, err)
			sp := &reincarnateLaunchCounter{}
			prev := agentd.Spawn
			agentd.Spawn = sp
			t.Cleanup(func() { agentd.Spawn = prev })

			resp := f.AsHuman().ReincarnateWith(src, map[string]any{"follow_up": "carry on"})

			assert.Equal(t, tc.status, resp.Code, "body=%s", resp.Raw)
			code, msg := errorBody(t, resp.Raw)
			assert.Equal(t, tc.code, code)
			assert.Contains(t, msg, tc.errorHas)
			assert.Zero(t, sp.launches, "nothing is launched")
			assert.Zero(t, agentd.AgentLaunchClaimsForTest(), "no launch claim is held")
			agents, err := db.ListActiveAgents()
			require.NoError(t, err)
			assert.Len(t, agents, len(agentsBefore), "no actor is added")
			sessions, err := db.ListSessions()
			require.NoError(t, err)
			assert.Len(t, sessions, len(sessionsBefore), "no session row is added")
			for _, k := range f.World.Tmux.Sent() {
				assert.NotContains(t, k.Text, "/exit", "nothing is soft-stopped")
			}
		})
	}
}
