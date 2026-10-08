package agentd_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func fedModelPolicy(t *testing.T, fh *fedHarness, upstream string) {
	t.Helper()
	token := filepath.Join(testutil.CanonicalTempDir(t), "credential")
	require.NoError(t, os.WriteFile(token, []byte("provider-secret-test"), 0600))
	_, err := config.Update(func(cfg *config.Config, err error) error {
		if err != nil {
			return err
		}
		if cfg.Agent == nil {
			cfg.Agent = &config.AgentConfig{}
		}
		cfg.Agent.HTTPProxies = map[string]config.HTTPProxyConfig{"model": {URL: upstream, Header: "X-Api-Key", HeaderValueFile: token, ModelPolicy: &config.ModelProxyPolicy{Enabled: true, Models: []string{"test-model"}, DailyRequests: 30, DailyTokens: 3000, PeerDailyRequests: 30, PeerDailyTokens: 3000, SessionDailyRequests: 30, SessionDailyTokens: 3000, MaxInputTokens: 50, MaxOutputTokens: 20, MaxConcurrent: 3, RequestsPerMinute: 30}}}
		return nil
	})
	require.NoError(t, err)
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermModelsProxy, "scope": "http_proxy=model"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
}
func fedModelFlow(t *testing.T, fh *fedHarness) *routebroker.FlowStream {
	t.Helper()
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	sid := proto.NewEnvelopeID()
	env := fh.peer.envelope(proto.KindModelOpen, proto.Endpoint{}, proto.ModelOpenPayload{Version: 1, Stream: sid, Proxy: "model", Session: "immutable-launch", Key: kp.Pub})
	env.From.Agent = ""
	fh.peer.send(env)
	var answer proto.ModelAnswerPayload
	fedEventually(t, "model answer", func() bool {
		for _, env := range fh.peer.envelopes(proto.KindModelAnswer) {
			var a proto.ModelAnswerPayload
			if env.DecodePayload(&a) == nil && a.Stream == sid {
				answer = a
				return true
			}
		}
		return false
	})
	require.True(t, answer.OK, answer.Reason)
	flow := routebroker.NewFlowStream(fedPeerStream(t, fh.peer, sid, kp, answer.Key, true))
	t.Cleanup(func() { _ = flow.Close() })
	return flow
}
func fedModelCall(t *testing.T, fh *fedHarness, body string) (int, string) {
	t.Helper()
	flow := fedModelFlow(t, fh)
	req, err := http.NewRequest(http.MethodPost, "http://model/v1/messages", strings.NewReader(body))
	require.NoError(t, err)
	req.Host = ""
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer never-forward-this")
	req.Header.Set("Cookie", "never-forward-cookie")
	require.NoError(t, req.Write(flow))
	require.NoError(t, flow.CloseWrite())
	resp, err := http.ReadResponse(bufio.NewReader(flow), req)
	require.NoError(t, err)
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	_ = flow.Close()
	return resp.StatusCode, string(data)
}
func TestFederation_ModelGatewayTokensSSECredentialsAndSwitch(t *testing.T) {
	fh := newFedHarness(t)
	var input atomic.Int64
	input.Store(10)
	var generations atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "provider-secret-test", r.Header.Get("X-Api-Key"))
		require.Empty(t, r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("Cookie"))
		if r.URL.Path == "/v1/messages/count_tokens" {
			_, _ = io.WriteString(w, `{"input_tokens":`+jsonNumber(input.Load())+`}`)
			return
		}
		generations.Add(1)
		var data struct {
			Stream   bool `json:"stream"`
			Messages any  `json:"messages"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&data))
		if data.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		_, _ = io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":10,"output_tokens":3}}`)
	}))
	defer upstream.Close()
	fedModelPolicy(t, fh, upstream.URL)
	body := `{"model":"test-model","max_tokens":5,"stream":true,"messages":[{"role":"user","content":"hello"}]}`
	status, result := fedModelCall(t, fh, body)
	require.Equal(t, 200, status, result)
	require.Contains(t, result, "message_stop")
	var usage []db.ModelProxyUsage
	fedEventually(t, "usage settled", func() bool {
		rec := fedHuman(t, fh.f, http.MethodGet, "/v1/models/usage", nil)
		if rec.Code != 200 {
			return false
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &usage)
		return len(usage) == 1 && usage[0].Complete
	})
	require.EqualValues(t, 13, usage[0].ChargedTokens)
	input.Store(51)
	status, result = fedModelCall(t, fh, body)
	require.Equal(t, 413, status, result)
	require.EqualValues(t, 1, generations.Load())
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/models/control", map[string]any{"name": "model", "peer": "bob", "disabled": true})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, fh.f, http.MethodGet, "/v1/models/control", nil)
	require.Equal(t, 200, rec.Code)
	require.False(t, bytes.Contains(rec.Body.Bytes(), []byte("provider-secret-test")))
	require.NotContains(t, rec.Body.String(), upstream.URL)
}
func jsonNumber(n int64) string { b, _ := json.Marshal(n); return string(b) }
