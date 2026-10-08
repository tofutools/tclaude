package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Opt-in native contract smoke: fake upstream only, isolated login and config,
// no operator credentials and no billable model requests.
func TestNativeCodexModelProxySavedLoginPrecedence(t *testing.T) {
	if os.Getenv("TCLAUDE_CODEX_PROXY_SMOKE") != "1" {
		t.Skip("set TCLAUDE_CODEX_PROXY_SMOKE=1 with Codex installed")
	}
	binary, err := exec.LookPath("codex")
	require.NoError(t, err)
	home := testutil.CanonicalTempDir(t)
	codexHome := filepath.Join(home, ".codex")
	require.NoError(t, os.MkdirAll(codexHome, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"id_token":"saved-login-canary","access_token":"saved-chatgpt-canary","refresh_token":"saved-refresh-canary","account_id":"test"},"last_refresh":"2099-01-01T00:00:00Z"}`), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("cli_auth_credentials_store=\"file\"\n"), 0600))
	var mu sync.Mutex
	requests := 0
	badAuth := false
	sawToolOutput := false
	denied := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer launch-only-canary" {
			badAuth = true
		}
		if r.URL.Path != "/v1/responses" {
			http.Error(w, "unsupported", 404)
			return
		}
		requests++
		if denied {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"message":"gateway denied smoke","type":"invalid_request_error","code":"invalid_api_key"}}`)
			return
		}
		var request struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Input json.RawMessage `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		if strings.Contains(string(request.Input), "function_call_output") {
			sawToolOutput = true
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			tool := ""
			args := ""
			for _, candidate := range request.Tools {
				switch candidate.Name {
				case "exec_command":
					tool = candidate.Name
					args = `{"cmd":"printf gateway-tool","max_output_tokens":100}`
				case "shell_command":
					tool = candidate.Name
					args = `{"command":"printf gateway-tool"}`
				case "shell":
					tool = candidate.Name
					args = `{"command":["sh","-c","printf gateway-tool"]}`
				}
			}
			if tool != "" {
				item := map[string]any{"id": "fc_test", "type": "function_call", "call_id": "call_test", "name": tool, "arguments": args}
				for _, e := range []map[string]any{{"type": "response.output_item.done", "output_index": 0, "item": item}, {"type": "response.completed", "response": map[string]any{"id": "resp_tool", "object": "response", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 4, "output_tokens": 2}}}} {
					data, _ := json.Marshal(e)
					_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e["type"], data)
				}
				return
			}
		}

		events := []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp_test", "object": "response", "status": "in_progress", "output": []any{}}},
			{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "msg_test", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}},
			{"type": "response.output_text.delta", "item_id": "msg_test", "output_index": 0, "content_index": 0, "delta": "gateway-ok"},
			{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"id": "msg_test", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "gateway-ok", "annotations": []any{}}}}},
			{"type": "response.completed", "response": map[string]any{"id": "resp_test", "object": "response", "status": "completed", "output": []any{map[string]any{"id": "msg_test", "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "gateway-ok", "annotations": []any{}}}}}, "usage": map[string]any{"input_tokens": 4, "output_tokens": 2, "total_tokens": 6}}},
		}
		for _, e := range events {
			data, _ := json.Marshal(e)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e["type"], data)
		}
	}))
	defer upstream.Close()
	env := []string{"HOME=" + home, "CODEX_HOME=" + codexHome, "PATH=" + os.Getenv("PATH"), "OPENAI_API_KEY=ambient-key-canary", "TCLAUDE_MODEL_PROXY_TOKEN=launch-only-canary"}
	h, err := harness.Resolve("codex")
	require.NoError(t, err)
	entries := []sandboxpolicy.EnvironmentEntry{}
	for _, pair := range env {
		k, v, _ := strings.Cut(pair, "=")
		entries = append(entries, sandboxpolicy.EnvironmentEntry{Name: k, Value: v})
	}
	transport, err := ResolveTclaudeLayerModelTransport(h, ModelTransportLaunchContext{ModelProxy: "fake@peer", Cwd: home, Environment: entries})
	require.NoError(t, err)
	require.True(t, transport.SessionGateway)
	require.NoError(t, validateCodexModelProxyEffective(env, "", "tclaude_gateway_smoke", upstream.URL+"/v1"))
	args := []string{"exec", "--skip-git-repo-check", "--json", "-m", "gpt-5.4", "-c", `approval_policy="never"`, "-c", `sandbox_mode="danger-full-access"`}
	for _, v := range harness.CodexModelProxyOverrides("tclaude_gateway_smoke", upstream.URL+"/v1") {
		args = append(args, "-c", v)
	}
	args = append(args, "Reply with exactly gateway-ok")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	cmd.Dir = home
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	require.Contains(t, string(out), "gateway-ok")
	threadID := ""
	for _, line := range strings.Split(string(out), "\n") {
		var event struct {
			Type   string `json:"type"`
			Thread string `json:"thread_id"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Type == "thread.started" {
			threadID = event.Thread
		}
	}
	require.NotEmpty(t, threadID)
	resumeArgs := []string{"exec", "resume", "--skip-git-repo-check", "--json", "-m", "gpt-5.4"}
	for _, v := range harness.CodexModelProxyOverrides("tclaude_gateway_smoke", upstream.URL+"/v1") {
		resumeArgs = append(resumeArgs, "-c", v)
	}
	resumeArgs = append(resumeArgs, threadID, "Reply gateway-ok again")
	resume := exec.CommandContext(ctx, binary, resumeArgs...)
	resume.Env = env
	resume.Dir = home
	resumed, e := resume.CombinedOutput()
	require.NoError(t, e, "%s", resumed)
	require.Contains(t, string(resumed), "gateway-ok")
	// app-server owns model execution for both API drive and its remote TUI.
	serverArgs := []string{}
	for _, v := range harness.CodexModelProxyOverrides("tclaude_gateway_smoke", upstream.URL+"/v1") {
		serverArgs = append(serverArgs, "-c", v)
	}
	serverArgs = append(serverArgs, "app-server", "--listen", "stdio://")
	serverCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	server := exec.CommandContext(serverCtx, binary, serverArgs...)
	server.Env = env
	server.Dir = home
	stdin, e := server.StdinPipe()
	require.NoError(t, e)
	stdout, e := server.StdoutPipe()
	require.NoError(t, e)
	server.Stderr = io.Discard
	require.NoError(t, server.Start())
	defer func() { _ = stdin.Close(); _ = server.Process.Kill(); _ = server.Wait() }()
	decoder := json.NewDecoder(bufio.NewReader(stdout))
	send := func(value any) { require.NoError(t, json.NewEncoder(stdin).Encode(value)) }
	readReply := func(id float64) map[string]any {
		for {
			var message map[string]any
			require.NoError(t, decoder.Decode(&message))
			if message["id"] == id {
				require.Nil(t, message["error"], "%v", message)
				return message
			}
		}
	}
	send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "tclaude-proxy-smoke", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}})
	readReply(1)
	send(map[string]any{"method": "initialized"})
	send(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": home, "model": "gpt-5.4", "approvalPolicy": "never", "sandbox": "danger-full-access"}})
	thread := readReply(2)["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	for turn := 0; turn < 2; turn++ {
		send(map[string]any{"id": 3 + turn, "method": "turn/start", "params": map[string]any{"threadId": thread, "input": []any{map[string]any{"type": "text", "text": "Reply gateway-ok", "text_elements": []any{}}}}})
		readReply(float64(3 + turn))
		for {
			var message map[string]any
			require.NoError(t, decoder.Decode(&message))
			if message["method"] == "turn/completed" {
				params := message["params"].(map[string]any)
				turn := params["turn"].(map[string]any)
				require.Equal(t, "completed", turn["status"], "%v", message)
				break
			}
		}
	}
	mu.Lock()
	denied = true
	mu.Unlock()
	refused := exec.CommandContext(ctx, binary, args...)
	refused.Env = env
	refused.Dir = home
	refusal, err := refused.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(refusal), "gateway denied smoke")
	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, requests, 6)
	require.True(t, sawToolOutput, "native Codex must execute the fake tool call and send its output on the next Responses request")
	require.False(t, badAuth)
	require.NotContains(t, strings.ToLower(string(out)), "saved-chatgpt-canary")
}
