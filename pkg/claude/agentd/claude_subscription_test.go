package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const testSubscriptionToken = "sk-ant-oat01-unit-test-not-real"

func clearClaudeSubscriptions() {
	claudeSubscriptions.Lock()
	claudeSubscriptions.agents = map[string]claudeSubscriptionHandoff{}
	claudeSubscriptions.requests = map[int64]claudeSubscriptionPending{}
	claudeSubscriptions.armed = map[string]bool{}
	claudeSubscriptions.Unlock()
}

func resetClaudeSubscriptions(t *testing.T) {
	t.Helper()
	clearClaudeSubscriptions()
	t.Cleanup(clearClaudeSubscriptions)
}

func TestClaudeSubscriptionTokenIsHandedToItsLaunchOnce(t *testing.T) {
	setupTestDB(t)
	resetClaudeSubscriptions(t)
	convID := "conv-sub"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, holdClaudeSubscription(agentID, claudeSubscriptionHandoff{peer: "inst_peer", token: testSubscriptionToken}))
	row := &db.SessionRow{ID: "launch-sub", ConvID: convID, Status: "idle"}
	fetch := func(row *db.SessionRow) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/models/claude-subscription", nil)
		if row != nil {
			r = r.WithContext(context.WithValue(r.Context(), httpProxyLaunchRowKey{}, row))
		}
		w := httptest.NewRecorder()
		handleClaudeSubscriptionToken(w, r)
		return w
	}
	require.Equal(t, 403, fetch(nil).Code, "only a verified live launch pane")
	require.Equal(t, 409, fetch(row).Code, "a launch tclaude did not start gets no token")
	// Every launch agentd starts (a resume reuses the same row ID) takes it once.
	for range 2 {
		args := clcommon.SpawnArgs{ConvID: convID}
		require.NoError(t, applyClaudeSubscription(&args))
		require.True(t, args.ClaudeSubscription)
		got := fetch(row)
		require.Equal(t, 200, got.Code, got.Body.String())
		require.Contains(t, got.Body.String(), testSubscriptionToken)
		require.Equal(t, 409, fetch(row).Code, "a tool in the pane cannot take it again")
	}
	other := &db.SessionRow{ID: "launch-other", ConvID: "conv-none", Status: "idle"}
	require.Equal(t, 404, fetch(other).Code)

	// A relaunch refuses once the token is no longer held.
	dropClaudeSubscription(agentID)
	args := clcommon.SpawnArgs{ConvID: convID}
	err = applyClaudeSubscription(&args)
	require.ErrorContains(t, err, "no longer holds the token")
	require.False(t, args.ClaudeSubscription)
	args = clcommon.SpawnArgs{ConvID: "conv-unmarked"}
	require.NoError(t, applyClaudeSubscription(&args), "an agent on this node's own login is untouched")
}

func TestClaudeSubscriptionForgottenOnRetire(t *testing.T) {
	setupTestDB(t)
	resetClaudeSubscriptions(t)
	agentID, _, err := db.EnsureAgentForConv("conv-ret", "test")
	require.NoError(t, err)
	require.NoError(t, holdClaudeSubscription(agentID, claudeSubscriptionHandoff{peer: "inst_peer", token: testSubscriptionToken}))
	_, ok := heldClaudeSubscription(agentID)
	require.True(t, ok)
	_, err = db.RetireAgentByID(agentID, "test", "done")
	require.NoError(t, err)
	_, ok = heldClaudeSubscription(agentID)
	require.False(t, ok)
}

func TestLiftClaudeSubscriptionToken(t *testing.T) {
	lift := func(body string) (string, string, int) {
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
		w := httptest.NewRecorder()
		token, ok := liftClaudeSubscriptionToken(w, r)
		if !ok {
			return "", "", w.Code
		}
		rest, _ := io.ReadAll(r.Body)
		return token, string(rest), 200
	}
	token, rest, code := lift(`{"name":"a","claude_subscription_token":"` + testSubscriptionToken + `"}`)
	require.Equal(t, 200, code)
	require.Equal(t, testSubscriptionToken, token)
	require.JSONEq(t, `{"name":"a"}`, rest)
	token, rest, code = lift(`{"name":"b"}`)
	require.Equal(t, 200, code)
	require.Empty(t, token)
	require.Equal(t, `{"name":"b"}`, rest, "a body without a token passes untouched")
	_, _, code = lift(`{"claude_subscription_token":"sk-ant-api03-key"}`)
	require.Equal(t, 400, code)
}

func TestClaudeSubscriptionPushOnlyForTheLocalOperator(t *testing.T) {
	setupTestDB(t)
	peer := &db.FederationPeer{InstanceID: "inst_b", Label: "b"}
	push := func(ctx context.Context, tail, body string) (string, error) {
		r := httptest.NewRequest(http.MethodPost, "/api/peer/b/"+tail, nil).WithContext(ctx)
		r.SetPathValue("tail", tail)
		out, err := prepareClaudeSubscriptionPush(r, []byte(body), peer)
		return string(out), err
	}
	out, err := push(context.Background(), "groups/crew/spawn", `{"brief":"x","claude_subscription":false,"claude_subscription_token":"smuggled"}`)
	require.NoError(t, err)
	require.JSONEq(t, `{"brief":"x"}`, out, "a page never supplies the token itself")
	out, err = push(context.Background(), "harnesses/operations", `{"claude_subscription":true}`)
	require.NoError(t, err)
	require.Equal(t, `{"claude_subscription":true}`, out, "only a spawn carries it")
	_, err = push(context.WithValue(context.Background(), remoteAuthedCtxKey{}, true), "groups/crew/spawn", `{"claude_subscription":true}`)
	require.ErrorContains(t, err, "only this node's own operator")
	_, err = push(context.Background(), "groups/crew/spawn", `{"claude_subscription":true}`)
	require.ErrorContains(t, err, "update tclaude there", "a peer without the capability gets no token")
}

func TestClaudeSubscriptionTokenStore(t *testing.T) {
	setupTestDB(t)
	call := func(method, body string) (int, map[string]any) {
		r := httptest.NewRequest(method, "/api/federation/claude-subscription", bytes.NewReader([]byte(body)))
		r = AsHumanPeer(r)
		w := httptest.NewRecorder()
		handleClaudeSubscription(w, r)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	code, out := call("GET", "")
	require.Equal(t, 200, code)
	require.Equal(t, false, out["set"])
	code, out = call("PUT", `{"token":"sk-ant-api03-key"}`)
	require.Equal(t, 400, code)
	require.Contains(t, out["error"], "API key")
	code, out = call("PUT", `{"token":" `+testSubscriptionToken+` "}`)
	require.Equal(t, 200, code)
	require.Equal(t, true, out["set"])
	require.NotContains(t, out, "token")
	info, err := os.Stat(claudeSubscriptionTokenPath())
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	token, _, err := readClaudeSubscriptionToken()
	require.NoError(t, err)
	require.Equal(t, testSubscriptionToken, token)
	code, out = call("DELETE", "")
	require.Equal(t, 200, code)
	require.Equal(t, false, out["set"])
}
