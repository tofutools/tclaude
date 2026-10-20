package agentd_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const fakeSubscriptionToken = "sk-ant-oat01-flowtest-not-a-real-token"

type recordingSpawner struct {
	inner agentd.Spawner
	mu    *sync.Mutex
	news  *[]clcommon.SpawnArgs
}

func (s recordingSpawner) SpawnNew(a clcommon.SpawnArgs) error {
	s.mu.Lock()
	*s.news = append(*s.news, a)
	s.mu.Unlock()
	return s.inner.SpawnNew(a)
}
func (s recordingSpawner) SpawnResume(a clcommon.SpawnArgs) error { return s.inner.SpawnResume(a) }

func recordSpawns(t *testing.T) func() []clcommon.SpawnArgs {
	t.Helper()
	var mu sync.Mutex
	var news []clcommon.SpawnArgs
	orig := agentd.Spawn
	agentd.Spawn = recordingSpawner{inner: orig, mu: &mu, news: &news}
	t.Cleanup(func() { agentd.Spawn = orig })
	return func() []clcommon.SpawnArgs {
		mu.Lock()
		defer mu.Unlock()
		return append([]clcommon.SpawnArgs(nil), news...)
	}
}

// An unrestricted peer's operator spawns here on their own Claude
// subscription: the token rides in the request, never reaches the spawn
// record, and the launch is marked to receive it through a pipe.
func TestPeerSpawnOnTheirClaudeSubscription(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	f.HaveGroup("crew")
	spawns := recordSpawns(t)
	setFedTrustLevel(t, fh, db.FederationTrustUnrestricted)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	post := func(body map[string]any) (int, string) {
		rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/groups/crew/spawn?as=local", body))
		return rec.Code, rec.Body.String()
	}
	code, body := post(map[string]any{"name": "helper", "initial_message": "hi", "claude_subscription_token": "sk-ant-api03-not-a-subscription"})
	require.Equal(t, 400, code, body)
	code, body = post(map[string]any{"name": "helper", "initial_message": "hi", "harness": "codex", "claude_subscription_token": fakeSubscriptionToken})
	require.NotEqual(t, 200, code, "a subscription runs Claude Code only: %s", body)

	code, body = post(map[string]any{"name": "helper", "initial_message": "hi", "claude_subscription_token": fakeSubscriptionToken})
	require.Equal(t, 200, code, body)
	require.NotContains(t, body, fakeSubscriptionToken)
	launched := spawns()
	require.NotEmpty(t, launched)
	last := launched[len(launched)-1]
	require.True(t, last.ClaudeSubscription, "the launch fetches the token in the pane")
	require.NotEmpty(t, last.ClaudeSubscriptionAgentID)
	marker, err := db.GetAgentClaudeSubscription(last.ClaudeSubscriptionAgentID)
	require.NoError(t, err)
	require.NotNil(t, marker)
	require.Equal(t, fh.peer.id.ID(), marker.Peer)

	// Neither the spawn audit nor any stored row holds the token.
	d, err := db.Open()
	require.NoError(t, err)
	var leaked int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE detail LIKE ?`, "%"+fakeSubscriptionToken+"%").Scan(&leaked))
	require.Zero(t, leaked)

	// The peer sees the agent as running on its own subscription.
	rec := testharness.Serve(h, testharness.JSONRequest(t, "GET", "/api/snapshot", nil))
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"claude_subscription":{"held":true,"label":"bob","peer":"`+fh.peer.id.ID()+`"}`)
	require.NotContains(t, rec.Body.String(), fakeSubscriptionToken)
}

// A restricted peer with a spawn grant sends a spawn request carrying the
// token: only the mode is stored, and the launch takes the token from memory.
func TestPeerSpawnRequestOnTheirClaudeSubscription(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("builders")
	spawns := recordSpawns(t)
	bob, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: bob.ID(), PubKey: bob.Pub, Label: "bob"}))
	grantSpawnFields(t, f, "bob", "builders", nil)

	code, _, errCode := peerSpawnPost(t, bob.ID(), "builders", map[string]any{"brief": "task", "claude_subscription_token": "not-a-token"})
	require.Equal(t, 400, code, errCode)
	code, id, _ := peerSpawnPost(t, bob.ID(), "builders", map[string]any{"brief": "task", "claude_subscription_token": fakeSubscriptionToken})
	require.Equal(t, 202, code)
	waitSpawnSettled(t, id)
	req, err := db.GetFederationSpawnRequest(id)
	require.NoError(t, err)
	require.Equal(t, "claude-subscription", req.Credentials, "only the mode is stored")
	require.Equal(t, db.FedSpawnApproved, req.Status, req.Reason)
	found := false
	for _, a := range spawns() {
		if a.ClaudeSubscription && a.ClaudeSubscriptionAgentID != "" {
			found = true
		}
	}
	require.True(t, found, "the approved launch runs on the subscription")
	require.False(t, strings.Contains(req.Brief+req.Reason, fakeSubscriptionToken))
}

// After the receiving daemon forgot the token, a relaunch refuses with a code
// the dashboard recognizes; the owner sends the token again and the agent
// resumes on it, or switches it to the node's own login.
func TestClaudeSubscriptionResendAndSwitchLogin(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	f.HaveGroup("crew")
	spawns := recordSpawns(t)
	setFedTrustLevel(t, fh, db.FederationTrustUnrestricted)
	h := agentd.PeerViewHandler(fh.peer.id.ID())
	call := func(method, path string, body any) (int, string) {
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, path, body))
		return rec.Code, rec.Body.String()
	}
	code, body := call("POST", "/api/groups/crew/spawn?as=local", map[string]any{"name": "helper", "initial_message": "hi", "claude_subscription_token": fakeSubscriptionToken})
	require.Equal(t, 200, code, body)
	agentID := spawns()[len(spawns())-1].ClaudeSubscriptionAgentID
	a, err := db.GetAgent(agentID)
	require.NoError(t, err)
	f.Stop(a.CurrentConvID, true)

	agentd.ForgetClaudeSubscriptionsForTest()
	code, body = call("POST", "/api/agents/"+agentID+"/resume", nil)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"action":"error:claude_subscription_lost"`)
	_, body = call("GET", "/api/snapshot", nil)
	require.Contains(t, body, `"held":false`)

	// Only a body carrying the token re-arms it; the owner's node fills it in.
	code, body = call("POST", "/api/agents/"+agentID+"/claude-subscription", map[string]any{})
	require.Equal(t, 400, code, body)
	code, body = call("POST", "/api/agents/"+agentID+"/claude-subscription", map[string]any{"claude_subscription_token": fakeSubscriptionToken})
	require.Equal(t, 200, code, body)
	require.NotContains(t, body, fakeSubscriptionToken)
	code, body = call("POST", "/api/agents/"+agentID+"/resume", nil)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"action":"resumed"`)

	// Switching to this node's own login drops the marker and the token.
	f.Stop(a.CurrentConvID, true)
	code, body = call("DELETE", "/api/agents/"+agentID+"/claude-subscription", nil)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"switched":true`)
	marker, err := db.GetAgentClaudeSubscription(agentID)
	require.NoError(t, err)
	require.Nil(t, marker)
	code, body = call("POST", "/api/agents/"+agentID+"/resume", nil)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"action":"resumed"`)
	_, body = call("GET", "/api/snapshot", nil)
	require.NotContains(t, body, `"claude_subscription"`)
}
