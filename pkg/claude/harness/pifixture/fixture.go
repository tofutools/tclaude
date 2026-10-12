// Package pifixture runs the real pi binary (harness.PiMinimumVersion)
// against a local OpenAI-compatible mock provider, credential-free, to pin
// what tclaude relies on: the status extension's payloads, and pi's session
// identity flags. It is opt-in (TCLAUDE_PI_FIXTURE_SMOKE=1) and runs in its
// own CI job, which installs the pinned pi from npm.
package pifixture

import (
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

	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

const SmokeEnvVar = "TCLAUDE_PI_FIXTURE_SMOKE"

// MockModel is the provider/id the mock serves.
const MockModel = "mock/mock-1"

// World is one hermetic pi installation: its own agent directory, a project,
// the mock provider, and a recorder standing in for `tclaude`.
type World struct {
	t        *testing.T
	Binary   string
	AgentDir string
	Project  string
	Recorder string
	Payloads string
	Mock     *Mock
}

// NewWorld skips unless the smoke is enabled and pi is installed.
func NewWorld(t *testing.T) *World {
	t.Helper()
	if os.Getenv(SmokeEnvVar) != "1" {
		t.Skipf("set %s=1 to run the real pi smoke", SmokeEnvVar)
	}
	bin, err := exec.LookPath("pi")
	if err != nil {
		t.Fatalf("%s=1 but pi is not on PATH: %v", SmokeEnvVar, err)
	}
	root := testutil.CanonicalTempDir(t)
	w := &World{
		t:        t,
		Binary:   bin,
		AgentDir: filepath.Join(root, "agent"),
		Project:  filepath.Join(root, "project"),
		Recorder: filepath.Join(root, "bin", "tclaude"),
		Payloads: filepath.Join(root, "payloads.jsonl"),
		Mock:     NewMock(t),
	}
	for _, dir := range []string{w.AgentDir, w.Project, filepath.Dir(w.Recorder)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	models := map[string]any{"providers": map[string]any{"mock": map[string]any{
		"baseUrl": w.Mock.URL + "/v1", "api": "openai-completions", "apiKey": "mock",
		"models": []map[string]any{{
			"id": "mock-1", "name": "Mock One", "contextWindow": 64000, "maxTokens": 4096,
			"reasoning": false, "input": []string{"text"},
			"cost": map[string]float64{"input": 1, "output": 2, "cacheRead": 0.1, "cacheWrite": 0},
		}},
	}}}
	raw, _ := json.Marshal(models)
	if err := os.WriteFile(filepath.Join(w.AgentDir, "models.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// One JSON document per callback, newline-terminated by the extension.
	script := "#!/bin/sh\n[ \"$1 $2\" = \"session hook-callback\" ] || exit 64\ncat >> " + shellQuote(w.Payloads) + "\n"
	if err := os.WriteFile(w.Recorder, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return w
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Env is the environment pi runs with: hermetic, offline, telemetry off.
func (w *World) Env(extra ...string) []string {
	env := []string{
		"HOME=" + filepath.Dir(w.AgentDir),
		"PATH=" + os.Getenv("PATH"),
		harness.PiAgentDirEnvVar + "=" + w.AgentDir,
		"PI_OFFLINE=1",
		"PI_SKIP_VERSION_CHECK=1",
		"PI_TELEMETRY=0",
		harness.PiHookBinEnvVar + "=" + w.Recorder,
	}
	return append(env, extra...)
}

// Run runs pi once in print mode from dir and returns its combined output.
// stdin is /dev/null: pi reads a piped stdin to its end before starting.
func (w *World) Run(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, w.Binary, args...)
	cmd.Dir = dir
	cmd.Env = w.Env()
	cmd.Stdin = nil
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// RecordedPayloads returns every payload the extension delivered, in order.
func (w *World) RecordedPayloads() []map[string]any {
	w.t.Helper()
	raw, err := os.ReadFile(w.Payloads)
	if err != nil && !os.IsNotExist(err) {
		w.t.Fatal(err)
	}
	var out []map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	for {
		var p map[string]any
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			w.t.Fatalf("recorded payloads are not JSON: %v\n%s", err, raw)
		}
		out = append(out, p)
	}
	return out
}

// Mock is a minimal streaming OpenAI chat-completions provider. A user
// message containing TOOL is answered with one bash tool call; anything else,
// including a tool result, with a short text reply.
type Mock struct {
	URL   string
	mu    sync.Mutex
	calls int
}

func NewMock(t *testing.T) *Mock {
	m := &Mock{}
	srv := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(srv.Close)
	m.URL = srv.URL
	return m
}

// Calls reports how many completions were requested.
func (m *Mock) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// ToolCommand is the command the mock's tool call runs.
const ToolCommand = "echo hello-from-tool"

func (m *Mock) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "mock-1", "object": "model"}}})
		return
	}
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	last := body.Messages[len(body.Messages)-1]
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(delta map[string]any, finish any) {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{
			"id": "c1", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": "mock-1",
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}))
	}
	if last.Role == "user" && len(body.Tools) > 0 && strings.Contains(string(last.Content), "TOOL") {
		chunk(map[string]any{"role": "assistant", "content": "Running a command."}, nil)
		chunk(map[string]any{"tool_calls": []map[string]any{{
			"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": "bash", "arguments": mustJSON(map[string]string{"command": ToolCommand})},
		}}}, nil)
		chunk(map[string]any{}, "tool_calls")
	} else {
		chunk(map[string]any{"role": "assistant", "content": "Done."}, nil)
		chunk(map[string]any{}, "stop")
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{
		"id": "c1", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": "mock-1",
		"choices": []any{},
		"usage":   map[string]any{"prompt_tokens": 1200, "completion_tokens": 30, "total_tokens": 1230},
	}))
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
