package geminifixture

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// SmokeEnvVar gates the suite: the scenarios run a third-party binary, so a
// plain `go test ./...` skips them.
const SmokeEnvVar = "TCLAUDE_GEMINI_FIXTURE_SMOKE"

// BinaryEnvVar optionally names the gemini executable; otherwise PATH is
// searched.
const BinaryEnvVar = "TCLAUDE_GEMINI_FIXTURE_BIN"

// Usage the mock reports for every model call, in the shape of the Gemini
// API's usageMetadata. promptTokenCount INCLUDES the cached tokens.
const (
	MockPromptTokens   = 1200
	MockCachedTokens   = 200
	MockOutputTokens   = 30
	MockThoughtsTokens = 10
)

// Mock is a loopback stand-in for the Gemini API's generateContent and
// streamGenerateContent endpoints. Every call answers Reply.
type Mock struct {
	Reply string

	mu       sync.Mutex
	requests []Request
}

// Request is one call the CLI made to the mock.
type Request struct {
	Path string
	Body string
}

func (m *Mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.requests = append(m.requests, Request{Path: r.URL.Path, Body: string(body)})
	m.mu.Unlock()
	chunk, _ := json.Marshal(map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": m.Reply}}},
			"finishReason": "STOP",
			"index":        0,
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount":        MockPromptTokens,
			"cachedContentTokenCount": MockCachedTokens,
			"candidatesTokenCount":    MockOutputTokens,
			"thoughtsTokenCount":      MockThoughtsTokens,
			"totalTokenCount":         MockPromptTokens + MockOutputTokens + MockThoughtsTokens,
		},
		"modelVersion": "gemini-2.5-flash",
	})
	if strings.Contains(r.URL.Path, ":streamGenerateContent") {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + string(chunk) + "\r\n\r\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(chunk)
}

// Requests returns the model calls made so far.
func (m *Mock) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Request(nil), m.requests...)
}

// ModelCalls returns the content-generation calls, ignoring any other request
// the CLI makes to the base URL.
func (m *Mock) ModelCalls() []Request {
	var calls []Request
	for _, req := range m.Requests() {
		if strings.Contains(req.Path, "generateContent") || strings.Contains(req.Path, "GenerateContent") {
			calls = append(calls, req)
		}
	}
	return calls
}

// World is one isolated Gemini installation: a private HOME holding
// ~/.gemini, a project directory, and the mock the CLI talks to. It points the
// process's HOME at that home too, so the tclaude code under test resolves the
// same ~/.gemini the CLI writes.
type World struct {
	T       *testing.T
	Bin     string
	Home    string
	Project string
	Mock    *Mock
	baseURL string
}

// NewWorld skips unless the smoke gate is set, and fails when it is set but no
// binary can be found.
func NewWorld(t *testing.T) *World {
	t.Helper()
	if os.Getenv(SmokeEnvVar) != "1" {
		t.Skipf("set %s=1 to run the real Gemini CLI", SmokeEnvVar)
	}
	bin := os.Getenv(BinaryEnvVar)
	if bin == "" {
		var err error
		bin, err = exec.LookPath("gemini")
		require.NoError(t, err, "%s is set but no gemini binary is on PATH", SmokeEnvVar)
	}
	// Resolve symlinks: Gemini keys its project registry by the real path, and
	// macOS temp dirs live behind /var -> /private/var.
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	home := filepath.Join(root, "home")
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".gemini"), 0o700))
	require.NoError(t, os.MkdirAll(project, 0o755))
	// The API-key auth type, chosen explicitly: with GOOGLE_GEMINI_BASE_URL
	// alone the CLI would infer its gateway auth type, which headless mode
	// refuses. The key route still honors the base URL.
	require.NoError(t, os.WriteFile(filepath.Join(home, ".gemini", "settings.json"),
		[]byte(`{"security":{"auth":{"selectedType":"gemini-api-key"}}}`+"\n"), 0o600))

	mock := &Mock{Reply: "fixture-ok"}
	server := httptest.NewServer(mock)
	t.Cleanup(server.Close)

	t.Setenv("HOME", home)
	t.Setenv("GEMINI_CLI_HOME", "")
	t.Setenv("GEMINI_CLI_TRUSTED_FOLDERS_PATH", "")
	// tclaude's database follows HOME; reopen it under this world's.
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	return &World{T: t, Bin: bin, Home: home, Project: project, Mock: mock, baseURL: server.URL}
}

// Result is one finished CLI run.
type Result struct {
	Stdout, Stderr string
	Err            error
}

// Run executes argv as built by tclaude (a leading `env K=V…` prefix
// included) in the project directory, with "gemini" resolved to the binary
// under test. extraEnv entries are appended to the scratch environment.
func (w *World) Run(argv []string, extraEnv ...string) Result {
	w.T.Helper()
	require.NotEmpty(w.T, argv, "tclaude built no argv")
	argv = append([]string(nil), argv...)
	for i, arg := range argv {
		if arg == "gemini" {
			argv[i] = w.Bin
			break
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = w.Project
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + w.Home,
		"TERM=dumb",
		"GEMINI_API_KEY=fixture-key",
		"GOOGLE_GEMINI_BASE_URL=" + w.baseURL,
	}, extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return Result{Stdout: stdout.String(), Stderr: stderr.String(), Err: err}
}

// RequireOK fails the test with the CLI's output when the run failed.
func (r Result) RequireOK(t *testing.T) {
	t.Helper()
	require.NoError(t, r.Err, "gemini failed\nstdout:\n%s\nstderr:\n%s", r.Stdout, r.Stderr)
}

// TrustWorkspaceEnv is the CLI's own per-launch trust override, for the
// scenarios that are not about folder trust.
const TrustWorkspaceEnv = "GEMINI_CLI_TRUST_WORKSPACE=true"
