package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Scenario (tcl-ncywjf): a no-copy clone of an OpenCode agent. OpenCode's
// conversation id is minted by the agentd-managed server its pane attaches
// to, so the clone needs its own server started before its fork.
//
// Expected: the clone comes up on a ses_ conversation its server issued, as
// a new agent (its own identity, not the source's), with its follow-up
// addressed to that conversation. Under tclaude's sandbox the clone's server
// gets private state of its own.
func TestClone_OpenCodeNoCopyRunsOnItsOwnManagedServer(t *testing.T) {
	for _, tc := range []string{"harness-builtin", "tclaude-layer"} {
		t.Run(tc, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("oc-crew")
			var extra map[string]any
			if tc == "tclaude-layer" {
				extra = map[string]any{"sandbox_implementation": "tclaude-layer",
					"approval": harness.OpenCodeApprovalAsk}
			}
			source := spawnOpenCodeAgent(t, f, "oc-crew", extra)

			const followUp = "PROBE-OC-CLONE: take the second half of the list"
			cloned := f.AsHuman().CloneWith(source.ConvID, map[string]any{
				"no_copy_conv": true, "follow_up": followUp,
			})
			require.Equalf(t, http.StatusOK, cloned.Code, "clone body=%s", cloned.Raw)
			require.True(t, strings.HasPrefix(cloned.NewConv, "ses_"),
				"the clone runs on its managed server's ses_ conversation, got %q", cloned.NewConv)
			assert.NotEqual(t, source.ConvID, cloned.NewConv)

			cloneAgent, err := db.AgentIDForConv(cloned.NewConv)
			require.NoError(t, err)
			require.NotEmpty(t, cloneAgent)
			assert.NotEqual(t, source.AgentID, cloneAgent, "a clone is a new agent")

			require.Eventually(t, func() bool {
				msgs, err := db.ListAgentMessagesForConv(cloned.NewConv, 50)
				require.NoError(t, err)
				for _, m := range msgs {
					if strings.Contains(m.Body, followUp) {
						return true
					}
				}
				return false
			}, 5*time.Second, 20*time.Millisecond, "the follow-up is addressed to the clone's real conversation")

			if tc == "tclaude-layer" {
				allocation, err := db.GetOpenCodeAgentStateAllocation(cloneAgent)
				require.NoError(t, err)
				require.NotNil(t, allocation, "the clone's server serves state private to the clone")
				source, err := db.GetOpenCodeAgentStateAllocation(source.AgentID)
				require.NoError(t, err)
				require.NotNil(t, source)
				assert.NotEqual(t, source.StateRoot, allocation.StateRoot)
			}
		})
	}
}

// Scenario: the clone's server cannot start. Expected: the clone fails and
// leaves nothing behind (no reserved actor, no private state).
func TestClone_OpenCodeServerStartFailureLeavesNothing(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("oc-crew")
	source := spawnOpenCodeAgent(t, f, "oc-crew", map[string]any{
		"sandbox_implementation": "tclaude-layer", "approval": harness.OpenCodeApprovalAsk,
	})
	t.Cleanup(agentd.SetOpenCodeRuntimeForTest(func(string, string, string, string) (agentd.OpenCodeRuntimeFixture, error) {
		return agentd.OpenCodeRuntimeFixture{}, os.ErrDeadlineExceeded
	}))
	sourceState, err := db.GetOpenCodeAgentStateAllocation(source.AgentID)
	require.NoError(t, err)
	require.NotNil(t, sourceState)
	stateBase := filepath.Dir(sourceState.StateRoot)
	before, err := os.ReadDir(stateBase)
	require.NoError(t, err)

	cloned := f.AsHuman().CloneWith(source.ConvID, map[string]any{"no_copy_conv": true, "follow_up": "go"})
	require.Equalf(t, http.StatusInternalServerError, cloned.Code, "clone body=%s", cloned.Raw)
	assert.Contains(t, string(cloned.Raw), "managed OpenCode server")

	after, err := os.ReadDir(stateBase)
	require.NoError(t, err)
	assert.Len(t, after, len(before), "the clone's private state was discarded")
}

// Scenario: an OpenCode no-copy clone under managed instructions. The clone
// now has a preset conversation, so it can bind its launch overlay (it was
// refused with a 409 while the id was unknown), and its standing guidance
// reaches it over the managed server's prompt API, not as an ignored launch
// argument.
func TestClone_OpenCodeNoCopyGetsManagedGuidanceOverTheServer(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("oc-crew")
	_, err := db.PutManagedInstruction(0, "rule", "operator", []byte("OC CLONE RULE"), false)
	require.NoError(t, err)
	source := spawnOpenCodeAgent(t, f, "oc-crew", nil)
	prompts := agentd.RecordOpenCodePromptsForTest(t)

	cloned := f.AsHuman().CloneWith(source.ConvID, map[string]any{"no_copy_conv": true, "follow_up": "go"})
	require.Equalf(t, http.StatusOK, cloned.Code, "clone body=%s", cloned.Raw)
	snapshot, err := db.GetManagedInstructionLaunch(mustAgentID(t, cloned.NewConv))
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	t.Cleanup(func() { _ = os.RemoveAll(snapshot.Root) })

	sent := prompts.WaitFor(t, cloned.NewConv, 10*time.Second)
	assert.Contains(t, sent, "OC CLONE RULE", "the guidance is the clone's first turn over the server")
}
