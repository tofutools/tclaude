package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/copilotapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Fake upstream only; isolated HOME and GitHub/provider canaries. COPILOT_OFFLINE
// makes saved GitHub accounts unavailable for fallback, including after a 401.
func TestNativeCopilotModelProxyNoFallback(t *testing.T) {
	if os.Getenv("TCLAUDE_COPILOT_PROXY_SMOKE") != "1" {
		t.Skip("set TCLAUDE_COPILOT_PROXY_SMOKE=1 with Copilot installed")
	}
	binary, err := exec.LookPath("copilot")
	require.NoError(t, err)
	require.NoError(t, validateCopilotModelProxyVersion(binary))
	home := testutil.CanonicalTempDir(t)
	copilotHome := filepath.Join(home, ".copilot")
	require.NoError(t, os.MkdirAll(copilotHome, 0700))
	// A saved GitHub account and a saved GitHub-routed model compete with BYOK.
	savedConfig, err := json.Marshal(map[string]any{"logged_in_users": []any{map[string]string{"host": "https://github.com", "login": "saved-login-canary"}}, "last_used_model": "claude-sonnet-4", "trustedFolders": []string{home}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(copilotHome, "config.json"), savedConfig, 0600))
	var mu sync.Mutex
	denied, badAuth, requests := false, false, 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer launch-only-canary" || r.URL.Path != "/v1/responses" {
			badAuth = true
		}
		requests++
		if denied {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"error":{"message":"gateway denied smoke","type":"invalid_request_error","code":"invalid_api_key"}}`)
			return
		}
		var input struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
		require.Equal(t, "gpt-5.4", input.Model)
		item := map[string]any{"id": "msg_smoke", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "gateway-ok", "annotations": []any{}}}}
		response := map[string]any{"id": "resp_smoke", "object": "response", "created_at": 1, "model": "gpt-5.4", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 4, "output_tokens": 2, "total_tokens": 6}}
		w.Header().Set("Content-Type", "text/event-stream")
		events := []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp_smoke", "object": "response", "status": "in_progress", "output": []any{}}},
			{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "msg_smoke", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}},
			{"type": "response.content_part.added", "item_id": "msg_smoke", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
			{"type": "response.output_text.delta", "item_id": "msg_smoke", "output_index": 0, "content_index": 0, "delta": "gateway-ok"},
			{"type": "response.output_text.done", "item_id": "msg_smoke", "output_index": 0, "content_index": 0, "text": "gateway-ok"},
			{"type": "response.output_item.done", "output_index": 0, "item": item},
			{"type": "response.completed", "response": response},
		}
		for _, event := range events {
			data, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
		}
	}))
	defer upstream.Close()
	env := []string{"HOME=" + home, "COPILOT_HOME=" + copilotHome, "PATH=" + os.Getenv("PATH"), "COPILOT_GITHUB_TOKEN=github-login-canary", "OPENAI_API_KEY=ambient-api-key-canary"}
	env = append(env, harness.CopilotModelProxyEnvironment(upstream.URL+"/v1", "launch-only-canary")...)
	run := func(extra ...string) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		args := []string{"-p", "Reply gateway-ok", "--model", "gpt-5.4", "--disable-builtin-mcps", "--output-format", "json", "--stream", "on"}
		args = append(args, extra...)
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = env
		cmd.Dir = home
		out, e := cmd.CombinedOutput()
		mu.Lock()
		refused := denied
		mu.Unlock()
		if refused {
			require.Error(t, e, "%s", out)
			require.Contains(t, string(out), "gateway denied smoke")
		} else {
			require.NoError(t, e, "%s", out)
			require.Contains(t, string(out), "gateway-ok")
		}
		return out
	}
	out := run()
	sessionID := ""
	for _, line := range strings.Split(string(out), "\n") {
		var event struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Type == "result" {
			sessionID = event.SessionID
		}
	}
	require.NotEmpty(t, sessionID)
	run("--resume=" + sessionID)
	// The interactive pane and API drive use the same --ui-server process. Verify
	// the native embedded RPC path rather than inferring it from prompt mode.
	t.Run("embedded-api", func(t *testing.T) {
		listener, e := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, e)
		port := listener.Addr().(*net.TCPAddr).Port
		require.NoError(t, listener.Close())
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, "--ui-server", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--model", "gpt-5.4", "--disable-builtin-mcps")
		command.Env = append(env, "TERM=xterm-256color")
		command.Dir = home
		terminal, e := pty.Start(command)
		require.NoError(t, e)
		go func() { _, _ = io.Copy(io.Discard, terminal) }()
		defer func() { _ = command.Process.Kill(); _ = command.Wait(); _ = terminal.Close() }()
		client, e := copilotapi.DialRetry(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil)
		require.NoError(t, e)
		defer client.Close()
		info, e := client.CreateSession(ctx, copilotapi.CreateSessionParams{SessionID: copilotapi.NewSessionID(), WorkingDirectory: home, ClientName: "tclaude", Streaming: true})
		require.NoError(t, e)
		for _, apiSession := range []string{info.SessionID, sessionID} {
			if apiSession == sessionID {
				resumed, e := client.ResumeSession(ctx, copilotapi.ResumeSessionParams{SessionID: sessionID, WorkingDirectory: home, ClientName: "tclaude", Streaming: true})
				require.NoError(t, e)
				require.Equal(t, sessionID, resumed.SessionID)
			}
			require.NoError(t, client.SetForegroundSession(ctx, apiSession))
			mu.Lock()
			before := requests
			mu.Unlock()
			_, e = client.Send(ctx, copilotapi.SendParams{SessionID: apiSession, Prompt: "Reply gateway-ok"})
			require.NoError(t, e)
			require.Eventually(t, func() bool {
				mu.Lock()
				advanced := requests > before
				mu.Unlock()
				metrics, e := client.UsageMetrics(ctx, apiSession)
				return advanced && e == nil && metrics.LastCallOutputTokens > 0
			}, 15*time.Second, 100*time.Millisecond)
		}
	})
	mu.Lock()
	denied = true
	mu.Unlock()
	run()
	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, requests, 3)
	require.False(t, badAuth, "all native requests must use the pinned Responses endpoint and launch bearer")
}
