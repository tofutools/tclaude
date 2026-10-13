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
	"net"
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
	// ExtraEnv is added to the environment of every later pi launch.
	ExtraEnv []string
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
	// Keep almost nothing verbatim, so even a one-turn session has history
	// to compact.
	if err := os.WriteFile(filepath.Join(w.AgentDir, "settings.json"),
		[]byte(`{"compaction":{"keepRecentTokens":50}}`), 0o600); err != nil {
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
	env = append(env, w.ExtraEnv...)
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
	URL string
	// Command is the command the tool call runs; "" is ToolCommand.
	Command  string
	mu       sync.Mutex
	calls    int
	messages []string
}

func NewMock(t *testing.T) *Mock {
	m := &Mock{}
	srv := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(srv.Close)
	m.URL = srv.URL
	return m
}

// NewMockAt is NewMock listening on addr (host:port), for a provider a
// sandboxed pi reaches over a routed address rather than loopback.
func NewMockAt(t *testing.T, addr string) *Mock {
	t.Helper()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	m := &Mock{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(m.serve))
	_ = srv.Listener.Close()
	srv.Listener = listener
	srv.Start()
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

// LastMessages returns the messages array of the latest completion request,
// as JSON: what pi sent the model.
func (m *Mock) LastMessages() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.messages) == 0 {
		return ""
	}
	return m.messages[len(m.messages)-1]
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
	m.messages = append(m.messages, mustJSON(body.Messages))
	m.mu.Unlock()
	last := body.Messages[len(body.Messages)-1]
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := func(delta map[string]any, finish any) {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{
			"id": "c1", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": "mock-1",
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}))
	}
	if last.Role == "user" && strings.Contains(string(last.Content), "SLOW") {
		// Hold the request open until the client gives up (an abort) or a
		// generous bound passes.
		select {
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
	if last.Role == "user" && len(body.Tools) > 0 && strings.Contains(string(last.Content), "TOOL") {
		chunk(map[string]any{"role": "assistant", "content": "Running a command."}, nil)
		chunk(map[string]any{"tool_calls": []map[string]any{{
			"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": "bash", "arguments": mustJSON(map[string]string{"command": m.toolCommand()})},
		}}}, nil)
		chunk(map[string]any{}, "tool_calls")
	} else {
		// Long enough that a short session still has history older than what
		// compaction keeps verbatim.
		chunk(map[string]any{"role": "assistant", "content": "Done. " + strings.Repeat("This is the mock reply. ", 20)}, nil)
		chunk(map[string]any{}, "stop")
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{
		"id": "c1", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": "mock-1",
		"choices": []any{},
		"usage":   map[string]any{"prompt_tokens": 12000, "completion_tokens": 30, "total_tokens": 12030},
	}))
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func (m *Mock) toolCommand() string {
	if m.Command != "" {
		return m.Command
	}
	return ToolCommand
}

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// RPC is pi running in `--mode rpc`: JSON commands on stdin, responses and
// events on stdout.
type RPC struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan map[string]any
	nextID int
	// backlog holds lines read while waiting for something else, so an
	// event that arrives before a command's response is not lost.
	backlog []map[string]any
}

// StartRPC launches pi in RPC mode from dir with the given extra arguments.
func (w *World) StartRPC(dir string, args ...string) *RPC {
	w.t.Helper()
	cmd := exec.Command(w.Binary, append([]string{"--mode", "rpc"}, args...)...)
	cmd.Dir = dir
	cmd.Env = w.Env()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		w.t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		w.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		w.t.Fatal(err)
	}
	r := &RPC{t: w.t, cmd: cmd, stdin: stdin, lines: make(chan map[string]any, 1024)}
	go func() {
		defer close(r.lines)
		dec := json.NewDecoder(stdout)
		for {
			var line map[string]any
			if err := dec.Decode(&line); err != nil {
				return
			}
			r.lines <- line
		}
	}()
	w.t.Cleanup(func() {
		if r.cmd.ProcessState == nil {
			_ = r.cmd.Process.Kill()
			_ = r.cmd.Wait()
		}
	})
	return r
}

// Send issues one command and returns its response.
func (r *RPC) Send(command map[string]any) map[string]any {
	r.t.Helper()
	r.nextID++
	id := fmt.Sprintf("req-%d", r.nextID)
	command["id"] = id
	raw, _ := json.Marshal(command)
	if _, err := r.stdin.Write(append(raw, '\n')); err != nil {
		r.t.Fatalf("send %v: %v", command, err)
	}
	return r.Await(func(line map[string]any) bool { return line["type"] == "response" && line["id"] == id })
}

// Reply writes one line that expects no response, such as an answer to an
// extension dialog (extension_ui_response).
func (r *RPC) Reply(line map[string]any) {
	r.t.Helper()
	raw, _ := json.Marshal(line)
	if _, err := r.stdin.Write(append(raw, '\n')); err != nil {
		r.t.Fatalf("reply %v: %v", line, err)
	}
}

// Await returns the first unconsumed line matching match, failing after a
// bound. Lines it passes over stay available to later calls.
func (r *RPC) Await(match func(map[string]any) bool) map[string]any {
	r.t.Helper()
	for i, line := range r.backlog {
		if match(line) {
			r.backlog = append(r.backlog[:i], r.backlog[i+1:]...)
			return line
		}
	}
	deadline := time.After(60 * time.Second)
	for {
		select {
		case line, ok := <-r.lines:
			if !ok {
				r.t.Fatal("pi exited while a reply was awaited")
			}
			if match(line) {
				return line
			}
			r.backlog = append(r.backlog, line)
		case <-deadline:
			r.t.Fatal("timed out waiting for pi")
		}
	}
}

// AwaitEvent returns the next event of the given type.
func (r *RPC) AwaitEvent(eventType string) map[string]any {
	r.t.Helper()
	return r.Await(func(line map[string]any) bool { return line["type"] == eventType })
}

// Close ends the session the way an RPC client does, by closing stdin, and
// waits for pi to exit.
func (r *RPC) Close() {
	r.t.Helper()
	_ = r.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- r.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		_ = r.cmd.Process.Kill()
		r.t.Fatal("pi did not exit after stdin closed")
	}
}
