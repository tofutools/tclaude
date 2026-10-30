package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func setSteward(t *testing.T, reads, writes string) {
	t.Helper()
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Steward = &config.StewardConfig{Reads: reads, Writes: writes}
	require.NoError(t, config.Save(cfg))
}

// A Steward's access comes from config, whatever the summon asks for, and any
// agent can reach a live one.
func TestSteward_AccessFromConfigAndReachableByAnyAgent(t *testing.T) {
	f, _ := processAuthoringFlow(t)
	enableCollab(t)
	stubScribeTerminal(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	summon := func() string {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", map[string]any{
			"name": "steward", "slugs": []string{agentd.PermNodeExec}, "scope": map[string]any{"kind": "steward"}, "brief": "Help.",
		})))
		require.Equalf(t, http.StatusOK, rec.Code, "summon body=%s", rec.Body.String())
		return decodeScribeResp(t, rec).ConvID
	}
	has := func(conv, slug string) bool {
		effect, ok, err := db.AgentPermissionOverride(conv, slug)
		require.NoError(t, err)
		return ok && effect == "grant"
	}
	// Workers outside the Steward's group: nobody can reach it yet.
	const worker = "stwd-aaaa-bbbb-cccc-000000000001"
	f.HaveGroup("team")
	f.HaveMember("team", worker)
	f.HaveAliveSession(worker, "stwd", "tclaude-stwd", f.TestCwd("work"))
	send := func(to string) int {
		r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/messages", map[string]any{"to": to, "body": "done"}), worker)
		return testharness.Serve(f.Mux, r).Code
	}
	require.Equal(t, http.StatusNotFound, send("steward"), "no live Steward")

	conv := summon()
	assert.True(t, has(conv, agentd.PermUsageRead), "operator-level reads by default")
	assert.True(t, has(conv, agentd.PermMessageDirect), "messaging by default")
	assert.False(t, has(conv, agentd.PermAgentSpawn), "no spawning by default")
	assert.False(t, has(conv, agentd.PermNodeExec), "a summon cannot ask for more")

	assert.Less(t, send("steward"), 300, "the Steward has an address")
	assert.Less(t, send(conv), 300, "and takes mail from any agent")
	rows, err := db.ListAgentMessagesForConv(conv, 10)
	require.NoError(t, err)
	delivered := 0
	for _, row := range rows {
		if row.Body == "done" {
			delivered++
		}
	}
	assert.Equal(t, 2, delivered, "the summon's brief is mail too")

	// Not an open door: only the Steward, only with Collab on.
	const other = "stwd-aaaa-bbbb-cccc-000000000002"
	f.HaveGroup("elsewhere")
	f.HaveMember("elsewhere", other)
	f.HaveAliveSession(other, "stwo", "tclaude-stwo", f.TestCwd("other"))
	assert.Equal(t, http.StatusForbidden, send(other))
	sessions, err := db.FindSessionsByConvID(conv)
	require.NoError(t, err)
	require.NotEmpty(t, sessions)
	f.MarkOffline(sessions[0].TmuxSession)
	assert.Equal(t, http.StatusForbidden, send(conv), "only a live Steward")

	setSteward(t, config.StewardReadsFleet, config.StewardWritesNone)
	conv = summon()
	assert.True(t, has(conv, agentd.PermFleetRead))
	assert.False(t, has(conv, agentd.PermUsageRead), "fleet-only reads")
	assert.False(t, has(conv, agentd.PermMessageDirect), "no writes")

	setSteward(t, "", config.StewardWritesWorkers)
	conv = summon()
	assert.True(t, has(conv, agentd.PermAgentSpawn))
}
