package agentd_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
)

func TestFederation_ModelGatewayOpenAIResponsesBudgetsAndErrors(t *testing.T) {
	fh := newFedHarness(t)
	var mode atomic.Value
	mode.Store("complete")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses", r.URL.Path)
		require.Equal(t, "provider-secret-test", r.Header.Get("X-Api-Key"))
		require.Empty(t, r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("Cookie"))
		var in map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		require.EqualValues(t, 20, in["max_output_tokens"])
		if mode.Load() == "error" {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"provider limit","param":null,"code":"limit"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")
		if mode.Load() != "truncated" {
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":3,\"input_tokens_details\":{\"cached_tokens\":7}}}}\n\n")
		}
	}))
	defer upstream.Close()
	fedModelPolicy(t, fh, upstream.URL)
	_, err := config.Update(func(cfg *config.Config, err error) error {
		if err != nil {
			return err
		}
		p := cfg.Agent.HTTPProxies["model"].ModelPolicy
		p.Dialect = "openai"
		p.PrecountInput = false
		return nil
	})
	require.NoError(t, err)
	call := func(path, body string) (int, string) {
		flow := fedModelFlow(t, fh, proto.ModelOpenPayload{Proxy: "model", Session: "immutable-launch", Dialect: "openai"})
		req, e := http.NewRequest(http.MethodPost, "http://model"+path, strings.NewReader(body))
		require.NoError(t, e)
		req.Host = ""
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer launch-canary")
		req.Header.Set("Cookie", "ambient-canary")
		require.NoError(t, req.Write(flow))
		require.NoError(t, flow.CloseWrite())
		resp, e := http.ReadResponse(bufio.NewReader(flow), req)
		require.NoError(t, e)
		defer resp.Body.Close()
		data, e := io.ReadAll(resp.Body)
		require.NoError(t, e)
		_ = flow.Close()
		return resp.StatusCode, string(data)
	}
	status, body := call("/v1/responses", `{"model":"test-model","stream":true,"input":"hello"}`)
	require.Equal(t, 200, status, body)
	require.Contains(t, body, "response.completed")
	fedEventually(t, "OpenAI usage settled", func() bool {
		u, e := db.ListModelProxyUsage(time.Now().UTC().Format("2006-01-02"))
		return e == nil && len(u) == 1 && u[0].Complete && u[0].ChargedTokens == 13
	})
	mode.Store("truncated")
	status, _ = call("/v1/responses", `{"model":"test-model","stream":true,"input":"hello"}`)
	require.Equal(t, 200, status)
	fedEventually(t, "incomplete reservation retained", func() bool {
		u, e := db.ListModelProxyUsage(time.Now().UTC().Format("2006-01-02"))
		if e != nil || len(u) != 2 {
			return false
		}
		for _, r := range u {
			if !r.Complete && r.ChargedTokens == 70 {
				return true
			}
		}
		return false
	})
	mode.Store("error")
	status, body = call("/v1/responses", `{"model":"test-model","stream":true,"input":"hello"}`)
	require.Equal(t, 429, status, body)
	require.Contains(t, body, "provider limit")
	require.NotContains(t, body, `"type":"error"`)
	for _, request := range []string{`{"model":"forbidden","input":"hello"}`, `{"model":"test-model","max_output_tokens":21}`, `{"model":"test-model","max_output_tokens":null}`} {
		status, body = call("/v1/responses", request)
		require.GreaterOrEqual(t, status, 400)
		require.Contains(t, body, `"error":`)
		require.NotContains(t, body, `"type":"error"`)
	}
	status, body = call("/v1/messages", `{"model":"test-model","max_tokens":5}`)
	require.Equal(t, 400, status, body)
}

func TestFederation_ModelGatewayDialectProbeDoesNotConsumeRequestCapacity(t *testing.T) {
	fh := newFedHarness(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"object":"response","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	fedModelPolicy(t, fh, upstream.URL)
	_, err := config.Update(func(cfg *config.Config, err error) error {
		if err != nil {
			return err
		}
		p := cfg.Agent.HTTPProxies["model"].ModelPolicy
		p.Dialect = "openai"
		p.PrecountInput = false
		p.RequestsPerMinute = 1
		p.MaxConcurrent = 1
		return nil
	})
	require.NoError(t, err)
	control := func(probe bool, dialect string) proto.ModelAnswerPayload {
		return fedModelControlAnswer(t, fh, proto.ModelOpenPayload{Proxy: "model", Session: "launch", Dialect: dialect, Probe: probe})
	}
	for range 3 {
		answer := control(true, "openai")
		require.True(t, answer.OK, answer.Reason)
		require.Empty(t, answer.Key)
	}
	require.False(t, control(true, "anthropic").OK, "dialect probe still applies policy")
	flow := fedModelFlow(t, fh, proto.ModelOpenPayload{Proxy: "model", Session: "immutable-launch", Dialect: "openai"})
	req, err := http.NewRequest(http.MethodPost, "http://model/v1/responses", strings.NewReader(`{"model":"test-model","input":"hello"}`))
	require.NoError(t, err)
	req.Host = ""
	require.NoError(t, req.Write(flow))
	require.NoError(t, flow.CloseWrite())
	resp, err := http.ReadResponse(bufio.NewReader(flow), req)
	require.NoError(t, err)
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	_ = flow.Close()
	require.Equal(t, 200, resp.StatusCode)
	answer := control(false, "openai")
	require.False(t, answer.OK)
	require.Contains(t, answer.Reason, "limit", "actual generation still consumes the single request slot")
}

func TestFederation_ModelGatewayResponseBeforeRequestHalfClose(t *testing.T) {
	for _, halfClose := range []bool{true, false} {
		t.Run(map[bool]string{true: "half-close", false: "deadline"}[halfClose], func(t *testing.T) {
			fh := newFedHarness(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"object":"response","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer upstream.Close()
			fedModelPolicy(t, fh, upstream.URL)
			_, err := config.Update(func(cfg *config.Config, err error) error {
				if err != nil {
					return err
				}
				p := cfg.Agent.HTTPProxies["model"].ModelPolicy
				p.Dialect = "openai"
				p.PrecountInput = false
				if !halfClose {
					p.MaxDurationSeconds = 1
				}
				return nil
			})
			require.NoError(t, err)
			flow := fedModelFlow(t, fh, proto.ModelOpenPayload{Proxy: "model", Session: "immutable-launch", Dialect: "openai"})
			req, err := http.NewRequest(http.MethodPost, "http://model/v1/responses", strings.NewReader(`{"model":"test-model","input":"hello"}`))
			require.NoError(t, err)
			require.NoError(t, req.Write(flow))
			// Deliberately receive the entire response, including the gateway's
			// half-close, before sending ours. HTTP body framing already ended the
			// request, so this ordering must neither stall the response nor abort us.
			response, err := io.ReadAll(flow)
			require.NoError(t, err)
			if halfClose {
				require.NoError(t, flow.CloseWrite())
			} else {
				// A peer that never half-closes still receives its response, and
				// the stream deadline bounds the gateway's wait for the missing FIN.
				select {
				case <-flow.Done():
				case <-time.After(5 * time.Second):
					t.Fatal("stream remained open after its deadline")
				}
			}
			resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(string(response))), req)
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
			require.Contains(t, string(body), `"status":"completed"`)
		})
	}
}

func fedModelControlAnswer(t *testing.T, fh *fedHarness, p proto.ModelOpenPayload) proto.ModelAnswerPayload {
	t.Helper()
	kp, err := stream.NewKeyPair()
	require.NoError(t, err)
	p.Version = 2
	p.Stream = proto.NewEnvelopeID()
	p.Key = kp.Pub
	env := fh.peer.envelope(proto.KindModelOpen, proto.Endpoint{}, p)
	env.From.Agent = ""
	fh.peer.send(env)
	var answer proto.ModelAnswerPayload
	fedEventually(t, "model control answer", func() bool {
		for _, env := range fh.peer.envelopes(proto.KindModelAnswer) {
			var a proto.ModelAnswerPayload
			if env.DecodePayload(&a) == nil && a.Stream == p.Stream {
				answer = a
				return true
			}
		}
		return false
	})
	return answer
}
