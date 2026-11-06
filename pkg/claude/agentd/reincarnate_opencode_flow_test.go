package agentd_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// spawnOpenCodeAgent spawns an OpenCode agent into group through the
// production spawn endpoint (its managed server is the flow fixture).
func spawnOpenCodeAgent(t *testing.T, f *testharness.Flow, group string, extra map[string]any) testharness.SpawnResp {
	t.Helper()
	t.Cleanup(agentd.SetOpenCodeAsyncSpawnResponseGraceForTest(30 * time.Second))
	body := map[string]any{
		"name":    "oc-worker",
		"harness": "opencode",
	}
	for k, v := range extra {
		body[k] = v
	}
	spawned := f.AsHuman().SpawnWith(group, body)
	require.Equal(t, http.StatusOK, spawned.Code, "raw=%s", spawned.Raw)
	require.True(t, strings.HasPrefix(spawned.ConvID, "ses_"), "spawn conv %q", spawned.ConvID)
	return spawned
}

// Scenario (tcl-yt3lyh): reincarnating an OpenCode agent. OpenCode's
// conversation id is minted by its agentd-managed server, so the successor
// cannot be preset with a fresh UUID the way a Claude Code successor is.
//
// Action: spawn an OpenCode agent, then reincarnate it with a follow-up: as
// itself (`tclaude agent reincarnate`, what the dashboard's default "ask the
// agent" mode leads to), as the operator, and as a managing agent with
// --target.
//
// Expected: the reincarnation succeeds, the successor runs on a ses_
// conversation its managed server issued and keeps the agent's identity, the
// handoff is addressed to that conversation, and the follow-up reaches it
// through the server's prompt API rather than as a launch argument.
func TestReincarnate_OpenCodeSuccessorUsesManagedServerConv(t *testing.T) {
	for _, tc := range []string{"self", "operator", "manager-target", "tclaude-layer"} {
		t.Run(tc, func(t *testing.T) {
			f := newFlow(t)
			g := f.HaveGroup("oc-crew")
			var extra map[string]any
			if tc == "tclaude-layer" {
				// tclaude's own sandbox: the successor's server needs its
				// private attachments directory before it starts.
				extra = map[string]any{"sandbox_implementation": "tclaude-layer",
					"approval": harness.OpenCodeApprovalAsk}
			}
			spawned := spawnOpenCodeAgent(t, f, "oc-crew", extra)
			prompts := agentd.RecordOpenCodePromptsForTest(t)

			const handoff = "PROBE-OC-HANDOFF: continue the parser fix"
			body := map[string]any{"follow_up": handoff}
			var rec testharness.ReincarnateResp
			switch tc {
			case "self":
				raw := agentReq(t, f, spawned.ConvID, http.MethodPost, "/v1/whoami/reincarnate", body)
				rec = testharness.ReincarnateResp{Code: raw.Code, Raw: raw.Body.Bytes()}
				decodeJSONInto(t, raw.Body.Bytes(), &rec)
			case "operator", "tclaude-layer":
				rec = f.AsHuman().ReincarnateWith(spawned.ConvID, body)
			case "manager-target":
				const manager = "oc-manager-aaaa-bbbb-cccc-111111111111"
				f.HaveAliveSession(manager, "oc-manager-session", "oc-manager-tmux", t.TempDir())
				f.HaveMember("oc-crew", manager)
				require.NoError(t, db.AddAgentGroupOwner(g.ID, manager, "test"))
				raw := agentReq(t, f, manager, http.MethodPost, "/v1/agent/"+spawned.ConvID+"/reincarnate", body)
				rec = testharness.ReincarnateResp{Code: raw.Code, Raw: raw.Body.Bytes()}
				decodeJSONInto(t, raw.Body.Bytes(), &rec)
			}
			require.Equal(t, http.StatusOK, rec.Code, "raw=%s", rec.Raw)
			require.True(t, strings.HasPrefix(rec.NewConv, "ses_"),
				"the successor runs on its managed server's ses_ conversation, got %q", rec.NewConv)
			assert.NotEqual(t, spawned.ConvID, rec.NewConv)

			msgID := handoffMessageIDFor(t, rec.NewConv)
			msg, err := db.GetAgentMessage(msgID)
			require.NoError(t, err)
			require.NotNil(t, msg)
			assert.Contains(t, msg.Body, handoff, "the handoff row is addressed to the successor's real conv")

			sent := prompts.WaitFor(t, rec.NewConv, 10*time.Second)
			assert.Contains(t, sent, handoff, "the follow-up reaches OpenCode through its prompt API")

			agentID, err := db.AgentIDForConv(rec.NewConv)
			require.NoError(t, err)
			assert.Equal(t, spawned.AgentID, agentID, "the successor keeps the agent's identity")
		})
	}
}

// Scenario: the OpenCode successor starts but its handoff cannot be sent
// through the managed server.
//
// Expected: the reincarnation fails instead of leaving an idle successor
// that never got its handoff; the successor is stopped and the predecessor
// is still the agent's live conversation.
func TestReincarnate_OpenCodeHandoffSendFailureKeepsPredecessor(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("oc-crew")
	spawned := spawnOpenCodeAgent(t, f, "oc-crew", nil)
	t.Cleanup(agentd.SetOpenCodePromptErrorForTest(errors.New("prompt_async refused")))

	sessionsBefore := len(f.World.Tmux.Sessions())
	rec := f.AsHuman().ReincarnateWith(spawned.ConvID, map[string]any{"follow_up": "continue"})
	require.Equal(t, http.StatusInternalServerError, rec.Code, "raw=%s", rec.Raw)
	assert.Contains(t, string(rec.Raw), "prompt_async refused")

	current, err := db.CurrentConvForAgent(spawned.AgentID)
	require.NoError(t, err)
	assert.Equal(t, spawned.ConvID, current, "the predecessor stays the agent's live conversation")
	assert.True(t, f.World.Tmux.IsAlive(spawned.TmuxSession), "the predecessor's pane is untouched")
	assert.Equal(t, sessionsBefore, len(f.World.Tmux.Sessions()), "the failed successor's pane was stopped")
}

func decodeJSONInto(t *testing.T, raw []byte, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(raw, v), "body=%s", raw)
}
