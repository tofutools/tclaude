package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const openCodeTestPermissionJSON = `[{"permission":"*","pattern":"*","action":"deny"},{"permission":"read","pattern":"*","action":"allow"}]`

func TestOpenCodeControlPlaneUsesBasicAuthAndMintsSession(t *testing.T) {
	const password = "private-password"
	var sawCreate bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != openCodeServerUsername || pass != password {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true}`))
		case "/session":
			sawCreate = true
			assert.Equal(t, "/tmp/project", r.URL.Query().Get("directory"))
			var body struct {
				Title      string                           `json:"title"`
				Permission []harness.OpenCodePermissionRule `json:"permission"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, "worker", body.Title)
			assert.Equal(t, []harness.OpenCodePermissionRule{
				{Permission: "*", Pattern: "*", Action: "deny"},
				{Permission: "read", Pattern: "*", Action: "allow"},
			}, body.Permission)
			_, _ = w.Write([]byte(`{"id":"ses_test123","permission":[{"permission":"*","pattern":"*","action":"deny"},{"permission":"read","pattern":"*","action":"allow"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	runtime := db.OpenCodeRuntime{
		PID: os.Getpid(), ServerURL: server.URL,
		Password: password, Cwd: "/tmp/project", PermissionJSON: openCodeTestPermissionJSON,
	}
	assert.True(t, openCodeHealthy(runtime))
	convID, err := createOpenCodeSession(runtime, "worker")
	require.NoError(t, err)
	assert.Equal(t, "ses_test123", convID)
	assert.True(t, sawCreate)
}

func TestOpenCodeSessionCreationFailsIfPolicyIsNotRetained(t *testing.T) {
	const password = "private-password"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ses_unprotected","permission":[]}`))
	}))
	defer server.Close()

	_, err := createOpenCodeSession(db.OpenCodeRuntime{
		PID: os.Getpid(), ServerURL: server.URL, Password: password,
		Cwd: "/tmp/project", PermissionJSON: openCodeTestPermissionJSON,
	}, "worker")
	require.ErrorContains(t, err, "did not retain")
}

func TestEnsureOpenCodeSessionPermissionRejectsLegacyEmptyPolicy(t *testing.T) {
	err := ensureOpenCodeSessionPermission(db.OpenCodeRuntime{})
	require.ErrorContains(t, err, "no persisted permission policy")
}

func TestOpenCodeHealthRequiresManagedListenerAndHealthyBody(t *testing.T) {
	const password = "private-password"
	healthCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthCalls++
		user, pass, ok := r.BasicAuth()
		require.True(t, ok)
		assert.Equal(t, openCodeServerUsername, user)
		assert.Equal(t, password, pass)
		if healthCalls < 3 {
			http.Error(w, "warming up", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"healthy":true}`))
	}))
	defer server.Close()

	runtime := db.OpenCodeRuntime{
		PID: os.Getpid(), ServerURL: server.URL, Password: password,
		PermissionJSON: openCodeTestPermissionJSON,
	}
	assert.True(t, openCodeProcessOwnsEndpoint(runtime.PID, runtime.ServerURL))
	const foreignPID = 99_999_999
	assert.False(t, openCodeProcessOwnsEndpoint(foreignPID, runtime.ServerURL))
	foreignRuntime := runtime
	foreignRuntime.PID = foreignPID
	assert.False(t, openCodeHealthyAfterRetries(foreignRuntime, 1, 0))
	_, err := createOpenCodeSession(foreignRuntime, "must-not-send")
	require.Error(t, err)
	err = sendOpenCodePrompt(&openCodeLaunch{
		ConvID: "ses_test", ServerURL: foreignRuntime.ServerURL,
		Password: foreignRuntime.Password, PID: foreignPID,
	}, "/tmp/project", "must-not-send", "", "")
	require.Error(t, err)
	assert.Zero(t, healthCalls, "credentials must not be sent to a listener owned by another PID")
	assert.True(t, openCodeHealthyAfterRetries(runtime, 3, time.Millisecond))
	assert.Equal(t, 3, healthCalls)

	bareOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer bareOK.Close()
	assert.False(t, openCodeHealthy(db.OpenCodeRuntime{
		PID: os.Getpid(), ServerURL: bareOK.URL, Password: password,
	}))
}

func TestEnsureOpenCodeSessionPermissionAppendsOnlyWhenSuffixMissing(t *testing.T) {
	const password = "private-password"
	current := []harness.OpenCodePermissionRule{{
		Permission: "bash", Pattern: "*", Action: "allow",
	}}
	patches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		require.True(t, ok)
		assert.Equal(t, password, pass)
		assert.Equal(t, "/session/ses_test", r.URL.Path)
		switch r.Method {
		case http.MethodGet:
		case http.MethodPatch:
			patches++
			var body struct {
				Permission []harness.OpenCodePermissionRule `json:"permission"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			current = append(current, body.Permission...)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"id": "ses_test", "permission": current,
		}))
	}))
	defer server.Close()

	runtime := db.OpenCodeRuntime{
		ConvID: "ses_test", PID: os.Getpid(), ServerURL: server.URL,
		Password: password, Cwd: "/tmp/project", PermissionJSON: openCodeTestPermissionJSON,
	}
	require.NoError(t, ensureOpenCodeSessionPermission(runtime))
	require.NoError(t, ensureOpenCodeSessionPermission(runtime))
	assert.Equal(t, 1, patches, "the exact suffix must not be appended repeatedly")
	expected, err := decodeOpenCodePermissionRules(openCodeTestPermissionJSON)
	require.NoError(t, err)
	assert.True(t, openCodePermissionHasSuffix(current, expected))
}

func TestReconcileOpenCodeRuntimeVerifiesPermissionOnHealthyServer(t *testing.T) {
	setupTestDB(t)
	const password = "private-password"
	patches := 0
	var current []harness.OpenCodePermissionRule
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true}`))
		case "/session/ses_reconcile":
			if r.Method == http.MethodPatch {
				patches++
				var body struct {
					Permission []harness.OpenCodePermissionRule `json:"permission"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				current = append(current, body.Permission...)
			}
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"id": "ses_reconcile", "permission": current,
			}))
		case "/event":
			http.Error(w, "closed", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: "spwn-reconcile", ConvID: "ses_reconcile",
		ServerURL: server.URL, Password: password, PID: os.Getpid(),
		Cwd: "/tmp/project", PermissionJSON: openCodeTestPermissionJSON,
	}))
	assert.True(t, reconcileOpenCodeRuntime("spwn-reconcile"))
	assert.Equal(t, 1, patches)

	openCodeProcesses.Lock()
	if process := openCodeProcesses.bySession["spwn-reconcile"]; process != nil && process.cancel != nil {
		process.cancel()
	}
	delete(openCodeProcesses.bySession, "spwn-reconcile")
	openCodeProcesses.Unlock()
}

func TestOpenCodeLaunchPromptCarriesModelAndVariant(t *testing.T) {
	const password = "private-password"
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		require.True(t, ok)
		assert.Equal(t, openCodeServerUsername, user)
		assert.Equal(t, password, pass)
		assert.Equal(t, "/session/ses_test/prompt_async", r.URL.Path)
		assert.Equal(t, "/tmp/project", r.URL.Query().Get("directory"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	err := sendOpenCodePrompt(&openCodeLaunch{
		ConvID: "ses_test", ServerURL: server.URL,
		Password: password, PID: os.Getpid(),
	}, "/tmp/project", "startup brief", "openai/gpt-5.6-terra", "high")
	require.NoError(t, err)
	assert.Equal(t, "high", body["variant"])
	assert.Equal(t, map[string]any{
		"providerID": "openai", "modelID": "gpt-5.6-terra",
	}, body["model"])
	parts := body["parts"].([]any)
	assert.Equal(t, "startup brief", parts[0].(map[string]any)["text"])
}

func TestOpenCodeSSEClientBoundsSetupWithoutWholeRequestTimeout(t *testing.T) {
	// The /event stream is long-lived, so a whole-request Timeout would sever a
	// healthy stream. The client must bound only setup: dial + response headers.
	assert.Zero(t, openCodeSSEHTTPClient.Timeout,
		"a whole-request timeout would kill a healthy SSE stream")
	transport, ok := openCodeSSEHTTPClient.Transport.(*http.Transport)
	require.True(t, ok, "the SSE client must use a bounded *http.Transport")
	assert.NotNil(t, transport.DialContext, "the SSE client must bound connection dial")
	assert.Equal(t, 10*time.Second, transport.ResponseHeaderTimeout,
		"the SSE client must bound the wait for response headers")
}

func TestOpenCodeRuntimeOwnsRecordedPID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()

	assert.True(t, openCodeRuntimeOwnsRecordedPID(db.OpenCodeRuntime{
		PID: os.Getpid(), ServerURL: server.URL,
	}), "the process owning the recorded endpoint must pass the recovered-pid gate")
	assert.False(t, openCodeRuntimeOwnsRecordedPID(db.OpenCodeRuntime{
		PID: 99_999_999, ServerURL: server.URL,
	}), "a pid that does not own the recorded endpoint must fail the gate (PID reuse)")
	assert.False(t, openCodeRuntimeOwnsRecordedPID(db.OpenCodeRuntime{
		PID: 1, ServerURL: server.URL,
	}), "pid<=1 must fail closed")
}

func TestFinishOpenCodeProcessExitCancelsSSEAndFlagsExit(t *testing.T) {
	const sessionID = "spwn-exit-cancel"
	ctx, cancel := context.WithCancel(context.Background())
	process := &openCodeProcess{cancel: cancel}
	openCodeProcesses.Lock()
	openCodeProcesses.bySession[sessionID] = process
	openCodeProcesses.Unlock()
	t.Cleanup(func() {
		openCodeProcesses.Lock()
		delete(openCodeProcesses.bySession, sessionID)
		openCodeProcesses.Unlock()
	})

	finishOpenCodeProcessExit(process, sessionID, 4242, nil, nil)

	openCodeProcesses.Lock()
	exited := process.exited
	openCodeProcesses.Unlock()
	assert.True(t, exited, "a server exit must flag the process")
	select {
	case <-ctx.Done():
	default:
		t.Fatal("a server exit must cancel the SSE consumer context")
	}
}

func TestEnsureOpenCodeSSESkipsAlreadyExitedProcess(t *testing.T) {
	const sessionID = "spwn-exited-nosse"
	process := &openCodeProcess{exited: true}
	openCodeProcesses.Lock()
	openCodeProcesses.bySession[sessionID] = process
	openCodeProcesses.Unlock()
	t.Cleanup(func() {
		openCodeProcesses.Lock()
		if p := openCodeProcesses.bySession[sessionID]; p != nil && p.cancel != nil {
			p.cancel()
		}
		delete(openCodeProcesses.bySession, sessionID)
		openCodeProcesses.Unlock()
	})

	ensureOpenCodeSSE(db.OpenCodeRuntime{
		SessionID: sessionID, ServerURL: "http://127.0.0.1:1", Cwd: "/tmp/project",
	})

	openCodeProcesses.Lock()
	started := process.cancel != nil
	openCodeProcesses.Unlock()
	assert.False(t, started,
		"a process that already died must not start a doomed SSE consumer")
}

func TestStopOpenCodeProcessVerifiesRecoveredPIDBeforeKill(t *testing.T) {
	prev := openCodeRuntimeVerified
	t.Cleanup(func() { openCodeRuntimeVerified = prev })
	var asked db.OpenCodeRuntime
	consulted := false
	openCodeRuntimeVerified = func(r db.OpenCodeRuntime) bool {
		asked = r
		consulted = true
		return false // not our managed server → the recovered pid must be spared
	}

	stopOpenCodeProcess(db.OpenCodeRuntime{
		SessionID: "spwn-recovered-kill", PID: 99_999_999,
		ServerURL: "http://127.0.0.1:2",
	}, nil)

	assert.True(t, consulted, "the recovered-pid path must consult the ownership gate")
	assert.Equal(t, 99_999_999, asked.PID, "the gate must be asked about the recorded pid")
}

func TestStopOpenCodeProcessNeverSelfKillsOnRecoveredPID(t *testing.T) {
	// Subtree endpoint ownership would match agentd's own pid (managed serves
	// are its children), so a stale row whose pid coincided with ours after
	// reuse must be short-circuited before the ownership gate — no self-kill.
	prev := openCodeRuntimeVerified
	t.Cleanup(func() { openCodeRuntimeVerified = prev })
	consulted := false
	openCodeRuntimeVerified = func(db.OpenCodeRuntime) bool {
		consulted = true
		return true
	}

	stopOpenCodeProcess(db.OpenCodeRuntime{
		SessionID: "spwn-self-pid", PID: os.Getpid(),
		ServerURL: "http://127.0.0.1:3",
	}, nil)

	assert.False(t, consulted,
		"a recorded pid equal to our own must be excluded before the ownership gate")
}

func TestRandomOpenCodePassword(t *testing.T) {
	first, err := randomOpenCodePassword()
	require.NoError(t, err)
	second, err := randomOpenCodePassword()
	require.NoError(t, err)
	assert.Len(t, first, 43)
	assert.NotEqual(t, first, second)
}

func TestOpenCodeCredentialHandoffNeverEntersWrapperArgv(t *testing.T) {
	args := sessionNewArgs(clcommon.SpawnArgs{
		Label:                  "spwn-test",
		Harness:                "opencode",
		OpenCodeServerURL:      "http://127.0.0.1:43210",
		OpenCodeServerPassword: "top-secret",
	})
	joined := strings.Join(args, " ")
	assert.NotContains(t, joined, "top-secret")
	assert.NotContains(t, joined, "43210")
}
