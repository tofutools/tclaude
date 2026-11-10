package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/statusbar"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These pin the identity proof the two brokered endpoints, /v1/whoami/hook
// and /v1/whoami/statusline, run before they apply anything: for every way a
// caller is turned away, the status, error code and message each endpoint
// answers, and the refusal reason the daemon logs, if any. Both endpoints run
// the same sequence and differ only in their nouns, so each case runs
// against both.

type brokerEndpoint struct {
	name, path string
	// noun is what the endpoint's own messages call its payload.
	noun string
	// body is a well-formed request claiming claimed for conv.
	body func(claimed, conv string) any
}

var brokerEndpoints = []brokerEndpoint{
	{name: "hook", path: "/v1/whoami/hook", noun: "hook", body: func(claimed, conv string) any {
		return session.BrokeredHookRequest{
			Input:            session.HookCallbackInput{ConvID: conv, HookEventName: "Stop"},
			ClaimedSessionID: claimed,
		}
	}},
	{name: "statusline", path: "/v1/whoami/statusline", noun: "statusline", body: func(claimed, conv string) any {
		return statusbar.BrokeredRenderRequest{
			ClaimedSessionID: claimed,
			RenderConvID:     conv,
			Payload:          statuslinePayload(conv, "Haiku 4.5", "claude-haiku-4-5", "low", 10, 1, 1, 200000, 0),
		}
	}},
}

const (
	// A plain (unsandboxed) session row and the caller under its pane.
	authDirectConv      = "d1000000-1111-2222-3333-444444444444"
	authDirectLabel     = "spwn-auth-direct"
	authDirectOther     = "d2000000-1111-2222-3333-444444444444"
	authDirectOtherLbl  = "spwn-auth-direct-other"
	authDirectCallerPID = 7300
	authDirectHarness   = 7301
	authDirectPanePID   = 7302

	// A caller no row can be found above, still under a live shell.
	authOrphanPID = 8400
	authShellPID  = 8399
)

// brokerAnswer is what an endpoint answered, and the refusal reasons logged.
type brokerAnswer struct {
	status        int
	code, message string
	reasons       []string
}

func postBroker(t *testing.T, f *testharness.Flow, ep brokerEndpoint, req *http.Request, logs *bytes.Buffer) brokerAnswer {
	t.Helper()
	rec := testharness.Serve(f.Mux, req)
	a := brokerAnswer{status: rec.Code}
	if rec.Code != http.StatusOK {
		var body struct{ Code, Error string }
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "%s: %s", ep.name, rec.Body.String())
		a.code, a.message = body.Code, body.Error
	}
	for _, line := range refusalLogLines(t, logs) {
		reason, _ := line["reason"].(string)
		a.reasons = append(a.reasons, reason)
	}
	return a
}

func brokerJSON(t *testing.T, ep brokerEndpoint, v any) *http.Request {
	t.Helper()
	return testharness.JSONRequest(t, http.MethodPost, ep.path, v)
}

func brokerRaw(ep brokerEndpoint, body []byte) *http.Request {
	return httptest.NewRequest(http.MethodPost, ep.path, bytes.NewReader(body))
}

// haveDirectSession records a plain session row keyed by its pane pid, the
// caller below its harness, and a second plain row the caller can claim.
func haveDirectSession(t *testing.T, f *testharness.Flow) {
	t.Helper()
	f.HaveAliveSession(authDirectConv, authDirectLabel, "tmux-auth-direct", f.World.HomeDir)
	row, err := db.LoadSession(authDirectLabel)
	require.NoError(t, err)
	require.NotNil(t, row)
	row.PID = authDirectPanePID
	require.NoError(t, db.SaveSession(row))
	f.HaveAliveSession(authDirectOther, authDirectOtherLbl, "tmux-auth-direct-other", f.World.HomeDir)
	t.Cleanup(agentd.SetProcTreeForTest(
		map[int]string{authDirectCallerPID: "tclaude", authDirectHarness: "node", authDirectPanePID: "sh"},
		map[int]int{authDirectCallerPID: authDirectHarness, authDirectHarness: authDirectPanePID},
	))
}

func haveOrphanCaller(t *testing.T) {
	t.Helper()
	t.Cleanup(agentd.SetProcTreeForTest(
		map[int]string{authOrphanPID: "node", authShellPID: "bash"}, map[int]int{authOrphanPID: authShellPID}))
}

func TestBrokerAuthenticate_EachRefusalOnBothEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name string
		// request sets up the world and builds the request.
		request func(t *testing.T, f *testharness.Flow, ep brokerEndpoint) *http.Request
		want    func(ep brokerEndpoint) brokerAnswer
	}{
		{
			name: "a well-proved layer caller is let through",
			request: func(t *testing.T, f *testharness.Flow, ep brokerEndpoint) *http.Request {
				caller := layerProcTree(t)
				haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
				return agentd.AsAgentPeerWithPID(brokerJSON(t, ep, ep.body(brokerLayerLabel, brokerLayerConv)), "", caller)
			},
			want: func(brokerEndpoint) brokerAnswer { return brokerAnswer{status: http.StatusOK} },
		},
		{
			name: "not a POST",
			request: func(t *testing.T, _ *testharness.Flow, ep brokerEndpoint) *http.Request {
				return agentd.AsAgentPeerWithPID(httptest.NewRequest(http.MethodGet, ep.path, nil), "", layerProcTree(t))
			},
			want: func(brokerEndpoint) brokerAnswer {
				return brokerAnswer{status: http.StatusMethodNotAllowed, code: "method", message: "POST only"}
			},
		},
		{
			name: "a caller with no peer at all",
			request: func(t *testing.T, _ *testharness.Flow, ep brokerEndpoint) *http.Request {
				return brokerJSON(t, ep, ep.body(brokerLayerLabel, brokerLayerConv))
			},
			want: func(brokerEndpoint) brokerAnswer {
				return brokerAnswer{status: http.StatusUnauthorized, code: "auth", message: "could not determine peer PID; refusing the request"}
			},
		},
		{
			name: "the operator, who is not a harness",
			request: func(t *testing.T, _ *testharness.Flow, ep brokerEndpoint) *http.Request {
				return agentd.AsHumanPeer(brokerJSON(t, ep, ep.body(brokerLayerLabel, brokerLayerConv)))
			},
			want: func(brokerEndpoint) brokerAnswer {
				return brokerAnswer{status: http.StatusForbidden, code: "unconfirmed", message: "unconfirmed caller: not a known agent, and no valid operator token. " +
					"If you are the human operator, set TCLAUDE_HUMAN_TOKEN to the operator token printed on the agentd startup banner, then retry."}
			},
		},
		{
			name: "a body over the cap",
			request: func(t *testing.T, f *testharness.Flow, ep brokerEndpoint) *http.Request {
				caller := layerProcTree(t)
				haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
				return agentd.AsAgentPeerWithPID(brokerRaw(ep, bytes.Repeat([]byte("x"), 10<<20+1)), "", caller)
			},
			want: func(ep brokerEndpoint) brokerAnswer {
				return brokerAnswer{status: http.StatusRequestEntityTooLarge, code: "body", message: ep.noun + " payload too large"}
			},
		},
		{
			name: "a body that is not JSON",
			request: func(t *testing.T, f *testharness.Flow, ep brokerEndpoint) *http.Request {
				caller := layerProcTree(t)
				haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
				return agentd.AsAgentPeerWithPID(brokerRaw(ep, []byte("{")), "", caller)
			},
			want: func(ep brokerEndpoint) brokerAnswer {
				return brokerAnswer{status: http.StatusBadRequest, code: "body", message: "malformed " + ep.noun + " payload"}
			},
		},
		{
			name: "a caller that has already exited",
			request: func(t *testing.T, f *testharness.Flow, ep brokerEndpoint) *http.Request {
				layerProcTree(t)
				haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
				const gonePID = 8500
				return agentd.AsAgentPeerWithPID(brokerJSON(t, ep, ep.body(brokerLayerLabel, brokerLayerConv)), "", gonePID)
			},
			want: func(brokerEndpoint) brokerAnswer {
				return brokerAnswer{status: http.StatusForbidden, code: "auth", message: "caller exited before its identity could be verified"}
			},
		},
		{
			name: "an unplaceable caller claiming a layer session",
			request: func(t *testing.T, f *testharness.Flow, ep brokerEndpoint) *http.Request {
				haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
				haveOrphanCaller(t)
				return agentd.AsAgentPeerWithPID(brokerJSON(t, ep, ep.body(brokerLayerLabel, brokerLayerConv)), "", authOrphanPID)
			},
			want: func(ep brokerEndpoint) brokerAnswer {
				return brokerAnswer{status: http.StatusForbidden, code: "auth", message: "claimed tclaude’s sandbox session does not own this caller",
					reasons: []string{ep.noun + ": tclaude’s sandbox caller failed live-pane proof"}}
			},
		},
		{
			name: "a layer caller claiming another layer session",
			request: func(t *testing.T, f *testharness.Flow, ep brokerEndpoint) *http.Request {
				caller := layerProcTree(t)
				haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
				haveLayerSession(t, f, pidReuseOtherConv, pidReuseOtherLabel, "tmux-pidreuse-other", brokerVictimPanePID)
				return agentd.AsAgentPeerWithPID(brokerJSON(t, ep, ep.body(pidReuseOtherLabel, pidReuseOtherConv)), "", caller)
			},
			want: func(ep brokerEndpoint) brokerAnswer {
				return brokerAnswer{status: http.StatusForbidden, code: "auth", message: "claimed tclaude’s sandbox session does not own this caller",
					reasons: []string{ep.noun + ": claimed tclaude’s sandbox session failed live-pane proof"}}
			},
		},
		{
			name: "an unplaceable caller with no claim",
			request: func(t *testing.T, f *testharness.Flow, ep brokerEndpoint) *http.Request {
				haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
				haveOrphanCaller(t)
				return agentd.AsAgentPeerWithPID(brokerJSON(t, ep, ep.body("", brokerLayerConv)), "", authOrphanPID)
			},
			want: func(ep brokerEndpoint) brokerAnswer {
				return brokerAnswer{status: http.StatusForbidden, code: "auth",
					message: "could not resolve a session row for this caller; refusing to apply its " + ep.noun,
					reasons: []string{ep.noun + ": caller could not be placed"}}
			},
		},
		{
			name: "an unplaceable caller claiming a session that does not exist",
			request: func(t *testing.T, f *testharness.Flow, ep brokerEndpoint) *http.Request {
				haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
				haveOrphanCaller(t)
				return agentd.AsAgentPeerWithPID(brokerJSON(t, ep, ep.body("spwn-no-such-row", brokerLayerConv)), "", authOrphanPID)
			},
			// Looking up a claimed row that is not there reports an error, so
			// the reason reads as a failed load (brokerClaimReason).
			want: func(ep brokerEndpoint) brokerAnswer {
				return brokerAnswer{status: http.StatusForbidden, code: "auth",
					message: "could not resolve a session row for this caller; refusing to apply its " + ep.noun,
					reasons: []string{ep.noun + ": claimed session could not be loaded to check the claim"}}
			},
		},
		{
			name: "a layer caller that omits its claim",
			request: func(t *testing.T, f *testharness.Flow, ep brokerEndpoint) *http.Request {
				caller := layerProcTree(t)
				haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
				return agentd.AsAgentPeerWithPID(brokerJSON(t, ep, ep.body("", brokerLayerConv)), "", caller)
			},
			want: func(ep brokerEndpoint) brokerAnswer {
				msg := "tclaude’s sandbox hook callback requires a proved session claim"
				if ep.name == "statusline" {
					msg = "tclaude’s sandbox statusline requires a proved session claim"
				}
				return brokerAnswer{status: http.StatusForbidden, code: "auth", message: msg,
					reasons: []string{ep.noun + ": tclaude’s sandbox callback omitted its session claim"}}
			},
		},
		{
			name: "a plain caller claiming another plain session",
			request: func(t *testing.T, f *testharness.Flow, ep brokerEndpoint) *http.Request {
				haveDirectSession(t, f)
				return agentd.AsAgentPeerWithPID(brokerJSON(t, ep, ep.body(authDirectOtherLbl, authDirectOther)), "", authDirectCallerPID)
			},
			want: func(ep brokerEndpoint) brokerAnswer {
				return brokerAnswer{status: http.StatusForbidden, code: "auth",
					message: "claimed session id does not match the session resolved for this caller",
					reasons: []string{ep.noun + ": claimed session id disagrees with the resolved row"}}
			},
		},
		{
			name: "a plain caller claiming its own session is let through",
			request: func(t *testing.T, f *testharness.Flow, ep brokerEndpoint) *http.Request {
				haveDirectSession(t, f)
				return agentd.AsAgentPeerWithPID(brokerJSON(t, ep, ep.body(authDirectLabel, authDirectConv)), "", authDirectCallerPID)
			},
			want: func(brokerEndpoint) brokerAnswer { return brokerAnswer{status: http.StatusOK} },
		},
	} {
		for _, ep := range brokerEndpoints {
			t.Run(ep.name+"/"+tc.name, func(t *testing.T) {
				t.Cleanup(agentd.ResetBrokerRefusalsForTest())
				t.Cleanup(agentd.ResetBrokerLimiterForTest())
				t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
				logs := captureDaemonLog(t)
				f := newFlow(t)

				got := postBroker(t, f, ep, tc.request(t, f, ep), logs)

				assert.Equal(t, tc.want(ep), got)
			})
		}
	}
}

// The two ceilings the shared sequence charges once it has a body: the
// identity-proof ceiling, always enforced, which an unplaceable caller hits
// on its 101st attempt in a second; and the per-row ceiling, enforced once
// the operator turns broker.enforce_limits on, which a placed caller hits on
// its 21st request.
func TestBrokerAuthenticate_TheCeilingsAnswerOnBothEndpoints(t *testing.T) {
	for _, ep := range brokerEndpoints {
		t.Run(ep.name+"/identity proofs", func(t *testing.T) {
			t.Cleanup(agentd.ResetBrokerRefusalsForTest())
			t.Cleanup(agentd.ResetBrokerLimiterForTest())
			t.Cleanup(agentd.FreezeBrokerLimiterClockForTest())
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			logs := captureDaemonLog(t)
			f := newFlow(t)
			haveOrphanCaller(t)

			for i := range 100 {
				got := postBroker(t, f, ep, agentd.AsAgentPeerWithPID(brokerJSON(t, ep, ep.body("", brokerLayerConv)), "", authOrphanPID), logs)
				require.Equal(t, http.StatusForbidden, got.status, "attempt %d is refused for identity, not rate", i+1)
			}
			logs.Reset()
			got := postBroker(t, f, ep, agentd.AsAgentPeerWithPID(brokerJSON(t, ep, ep.body("", brokerLayerConv)), "", authOrphanPID), logs)
			assert.Equal(t, brokerAnswer{status: http.StatusTooManyRequests, code: "rate", message: "too many identity proof attempts"}, got)
		})
		t.Run(ep.name+"/per row", func(t *testing.T) {
			t.Cleanup(agentd.ResetBrokerRefusalsForTest())
			t.Cleanup(agentd.ResetBrokerLimiterForTest())
			t.Cleanup(agentd.FreezeBrokerLimiterClockForTest())
			t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
			logs := captureDaemonLog(t)
			f := newFlow(t)
			cfg := config.DefaultConfig()
			cfg.Broker = &config.BrokerConfig{EnforceLimits: true}
			require.NoError(t, config.Save(cfg))
			agentd.ResetBrokerLimiterForTest()
			haveDirectSession(t, f)

			for i := range 20 {
				got := postBroker(t, f, ep, agentd.AsAgentPeerWithPID(brokerJSON(t, ep, ep.body(authDirectLabel, authDirectConv)), "", authDirectCallerPID), logs)
				require.Equal(t, http.StatusOK, got.status, "request %d is under the ceiling", i+1)
			}
			got := postBroker(t, f, ep, agentd.AsAgentPeerWithPID(brokerJSON(t, ep, ep.body(authDirectLabel, authDirectConv)), "", authDirectCallerPID), logs)
			msg := "too many hook events"
			if ep.name == "statusline" {
				msg = "too many statusline renders"
			}
			assert.Equal(t, brokerAnswer{status: http.StatusTooManyRequests, code: "rate", message: msg}, got)
		})
	}
}
