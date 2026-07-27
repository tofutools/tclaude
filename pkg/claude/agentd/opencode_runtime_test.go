package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
	"github.com/tofutools/tclaude/pkg/claude/session"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

func TestOpenCodeHealthyUnixTransportNeverDialsLogicalHostTCP(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("OpenCode Unix transport is Linux-only")
	}
	var tcpCalls atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		tcpCalls.Add(1)
	}))
	defer trap.Close()
	root := filepath.Join(agentipctest.ShortSocketDir(t),
		"agt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, os.Mkdir(root, 0o700))
	socketPath := filepath.Join(root, "control.sock")
	listener, device, inode, err := opencodeapi.CreateUnixListener(socketPath)
	require.NoError(t, err)
	unixServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		require.True(t, ok)
		assert.Equal(t, openCodeServerUsername, username)
		assert.Equal(t, "secret", password)
		_, _ = w.Write([]byte(`{"healthy":true}`))
	})}
	go func() { _ = unixServer.Serve(listener) }()
	t.Cleanup(func() {
		_ = unixServer.Close()
		_ = os.Remove(socketPath)
	})
	runtime := db.OpenCodeRuntime{
		PID: os.Getpid(), ServerURL: trap.URL, Password: "secret",
		Transport:         db.OpenCodeTransportUnixRelay,
		ControlSocketPath: socketPath, ControlSocketDevice: device,
		ControlSocketInode: inode,
	}
	require.True(t, openCodeHealthy(runtime))
	assert.Zero(t, tcpCalls.Load())
}

const openCodeTestPermissionJSON = `[{"permission":"*","pattern":"*","action":"deny"},{"permission":"read","pattern":"*","action":"allow"}]`

type openCodeBlockingReadCloser struct {
	closed chan struct{}
	once   sync.Once
}

func (b *openCodeBlockingReadCloser) Read([]byte) (int, error) {
	<-b.closed
	return 0, context.Canceled
}

func (b *openCodeBlockingReadCloser) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

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

func TestOpenCodeSessionCreationReportsBoundedServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "private state is not writable", http.StatusInternalServerError)
	}))
	defer server.Close()

	_, err := createOpenCodeSession(db.OpenCodeRuntime{
		PID: os.Getpid(), ServerURL: server.URL, Password: "private-password",
		Cwd: "/tmp/project", PermissionJSON: openCodeTestPermissionJSON,
	}, "worker")
	require.ErrorContains(t, err, "HTTP 500: private state is not writable")
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

func TestStopOpenCodeProcessJoinsSSEProjector(t *testing.T) {
	const sessionID = "spwn-stop-joins-sse"
	ctx, cancel := context.WithCancel(context.Background())
	projectorStopped := make(chan struct{})
	releaseProjector := make(chan struct{})
	process := &openCodeProcess{cancel: cancel, sseDone: projectorStopped}
	go func() {
		<-ctx.Done()
		<-releaseProjector
		close(projectorStopped)
	}()
	openCodeProcesses.Lock()
	openCodeProcesses.bySession[sessionID] = process
	openCodeProcesses.Unlock()

	stopReturned := make(chan struct{})
	go func() {
		stopOpenCodeProcess(db.OpenCodeRuntime{SessionID: sessionID}, nil)
		close(stopReturned)
	}()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel the SSE projector")
	}
	select {
	case <-stopReturned:
		t.Fatal("stop returned before the SSE projector exited")
	case <-time.After(25 * time.Millisecond):
	}
	ensureOpenCodeSSE(db.OpenCodeRuntime{
		SessionID: sessionID, ServerURL: "http://127.0.0.1:1", Cwd: "/tmp/project",
	})
	openCodeProcesses.Lock()
	registered := openCodeProcesses.bySession[sessionID]
	stopping := registered != nil && registered.stopping
	openCodeProcesses.Unlock()
	assert.Same(t, process, registered,
		"concurrent SSE registration must retain the stopping process tombstone")
	assert.True(t, stopping)
	close(releaseProjector)
	select {
	case <-stopReturned:
	case <-time.After(time.Second):
		t.Fatal("stop did not return after the SSE projector exited")
	}
	openCodeProcesses.Lock()
	_, registeredAfterStop := openCodeProcesses.bySession[sessionID]
	openCodeProcesses.Unlock()
	assert.False(t, registeredAfterStop, "the stopping tombstone is removed after projector join")
}

func openCodeProjectorApplyLockExists(runtime db.OpenCodeRuntime) bool {
	openCodeProjectorApplyLocksMu.Lock()
	defer openCodeProjectorApplyLocksMu.Unlock()
	return openCodeProjectorApplyLocks[openCodeProjectorApplyKey(runtime)] != nil
}

func TestStoppedOpenCodeProjectorReleasesConversationCaches(t *testing.T) {
	resetOpenCodeVirtualCostStateForTest()
	t.Cleanup(resetOpenCodeVirtualCostStateForTest)
	const sessionID, convID = "spwn-stop-cleans-cache", "ses-stop-cleans-cache"
	runtime := db.OpenCodeRuntime{SessionID: sessionID, ConvID: convID}
	openCodeVirtualCostState.Lock()
	rememberOpenCodeKnownStepLocked(convID, "msg-cleanup", "part-cleanup")
	rememberOpenCodeSnapshotStepLocked(convID, "msg-cleanup", "part-cleanup")
	openCodeVirtualCostState.Unlock()
	require.True(t, withOpenCodeProjectorApplyLock(context.Background(), runtime, func() {}))
	require.True(t, openCodeProjectorApplyLockExists(runtime))

	originalDelay := openCodeConversationStateCleanupDelay
	openCodeConversationStateCleanupDelay = 10 * time.Millisecond
	t.Cleanup(func() {
		openCodeConversationStateCleanupDelay = originalDelay
		openCodeProjectorApplyLocksMu.Lock()
		delete(openCodeProjectorApplyLocks, openCodeProjectorApplyKey(runtime))
		openCodeProjectorApplyLocksMu.Unlock()
	})
	projectorStopped := make(chan struct{})
	close(projectorStopped)
	process := &openCodeProcess{sseDone: projectorStopped}
	openCodeProcesses.Lock()
	openCodeProcesses.bySession[sessionID] = process
	openCodeProcesses.Unlock()

	stopOpenCodeProcess(runtime, process)

	require.Eventually(t, func() bool {
		openCodeVirtualCostState.Lock()
		known := len(openCodeVirtualCostState.knownSteps[convID])
		snapshot := len(openCodeVirtualCostState.snapshotSteps[convID])
		openCodeVirtualCostState.Unlock()
		return known == 0 && snapshot == 0 && !openCodeProjectorApplyLockExists(runtime)
	}, time.Second, 5*time.Millisecond,
		"a joined, inactive conversation releases its step caches and projector lock")
}

func TestOpenCodeSSEBodyIsActivelyClosedOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	body := &openCodeBlockingReadCloser{closed: make(chan struct{})}
	stopWatching := closeOpenCodeSSEBodyOnCancel(ctx, body)
	cancel()
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("SSE response body was not actively closed after cancellation")
	}
	stopWatching()
}

func TestStopOpenCodeProcessBoundsStuckSSEProjectorJoin(t *testing.T) {
	const sessionID = "spwn-stop-bounds-stuck-sse"
	ctx, cancel := context.WithCancel(context.Background())
	process := &openCodeProcess{cancel: cancel, sseDone: make(chan struct{})}
	openCodeProcesses.Lock()
	openCodeProcesses.bySession[sessionID] = process
	openCodeProcesses.Unlock()

	started := time.Now()
	stopReturned := make(chan struct{})
	go func() {
		stopOpenCodeProcess(db.OpenCodeRuntime{SessionID: sessionID}, nil)
		close(stopReturned)
	}()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel the stuck SSE projector")
	}
	openCodeProcesses.Lock()
	registered := openCodeProcesses.bySession[sessionID]
	openCodeProcesses.Unlock()
	require.Same(t, process, registered,
		"stopping tombstone remains registered during the bounded join")
	assert.True(t, registered.stopping)

	select {
	case <-stopReturned:
	case <-time.After(openCodeProcessStopWait + time.Second):
		t.Fatal("stop remained blocked after the bounded SSE-projector join")
	}
	assert.GreaterOrEqual(t, time.Since(started), openCodeProcessStopWait,
		"stop must still give the projector its bounded grace period")
	openCodeProcesses.Lock()
	_, registeredAfterTimeout := openCodeProcesses.bySession[sessionID]
	openCodeProcesses.Unlock()
	assert.False(t, registeredAfterTimeout,
		"stopping tombstone is safely removed after the bounded join expires")
}

func TestOpenCodeProjectorApplyLockFollowsConversationAcrossResume(t *testing.T) {
	oldRuntime := db.OpenCodeRuntime{SessionID: "spawn", ConvID: "ses-shared"}
	resumedRuntime := db.OpenCodeRuntime{SessionID: "resume", ConvID: "ses-shared"}
	oldEntered := make(chan struct{})
	releaseOld := make(chan struct{})
	oldDone := make(chan struct{})
	go func() {
		withOpenCodeProjectorApplyLock(context.Background(), oldRuntime, func() {
			close(oldEntered)
			<-releaseOld
		})
		close(oldDone)
	}()
	<-oldEntered

	resumedEntered := make(chan struct{})
	go withOpenCodeProjectorApplyLock(context.Background(), resumedRuntime, func() {
		close(resumedEntered)
	})
	select {
	case <-resumedEntered:
		t.Fatal("resumed local session overtook the old projector for the same conversation")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseOld)
	select {
	case <-oldDone:
	case <-time.After(time.Second):
		t.Fatal("old conversation projector did not release its apply lock")
	}
	select {
	case <-resumedEntered:
	case <-time.After(time.Second):
		t.Fatal("resumed projector did not acquire the conversation lock after release")
	}
}

func TestOpenCodeProjectorApplyLockBoundsStuckGeneration(t *testing.T) {
	oldRuntime := db.OpenCodeRuntime{SessionID: "spawn-stuck", ConvID: "ses-stuck"}
	resumedRuntime := db.OpenCodeRuntime{SessionID: "resume-stuck", ConvID: "ses-stuck"}
	oldEntered := make(chan struct{})
	releaseOld := make(chan struct{})
	go withOpenCodeProjectorApplyLock(context.Background(), oldRuntime, func() {
		close(oldEntered)
		<-releaseOld
	})
	<-oldEntered

	started := time.Now()
	entered := false
	acquired := withOpenCodeProjectorApplyLock(context.Background(), resumedRuntime, func() {
		entered = true
	})
	assert.False(t, acquired)
	assert.False(t, entered)
	assert.GreaterOrEqual(t, time.Since(started), openCodeProcessStopWait)
	assert.Less(t, time.Since(started), openCodeProcessStopWait+time.Second,
		"replacement lock acquisition must be bounded")
	close(releaseOld)
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

func TestOpenCodeRuntimeSandboxSpecRoundTripsAndRevalidates(t *testing.T) {
	setupTestDB(t)
	home, err := filepath.EvalSymlinks(os.Getenv("HOME"))
	require.NoError(t, err)
	t.Setenv("HOME", home)
	cwd := filepath.Join(home, "project")
	require.NoError(t, os.MkdirAll(cwd, 0o700))
	snapshot := sandboxpolicy.EmptySnapshot()
	spec, err := session.BuildTclaudeLayerLaunchSpec(session.TclaudeLayerLaunchInput{
		HarnessName: harness.OpenCodeName,
		Cwd:         cwd,
		Snapshot:    &snapshot,
	})
	require.NoError(t, err)
	spec.Version = session.TclaudeLayerLegacyLaunchSpecVersion
	implementation, encoded, err := openCodeSandboxRecord(&spec)
	require.NoError(t, err)

	decoded, err := openCodeRuntimeSandboxSpec(db.OpenCodeRuntime{
		Cwd:                   cwd,
		SandboxImplementation: implementation,
		SandboxLaunchSpecJSON: encoded,
	})
	require.NoError(t, err)
	require.NotNil(t, decoded)
	assert.Equal(t, spec, *decoded)
	assert.Empty(t, decoded.Contract.PrivateWriteDirs,
		"a new binary must accept pre-field v2 specs with no private roots")
	protectedPaths, err := sandboxpolicy.ProtectedPaths()
	require.NoError(t, err)
	privateParentProtected := false
	for _, protected := range protectedPaths {
		if sandboxpolicy.PathContainsOrEqual(
			protected,
			tclcommon.SpawnAttachmentsPrivateBase(),
		) {
			privateParentProtected = true
			break
		}
	}
	assert.True(t, privateParentProtected,
		"pre-field v2 servers must inherit the existing daemon-data hide over the private parent")

	_, err = openCodeRuntimeSandboxSpec(db.OpenCodeRuntime{
		Cwd:                   cwd,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
	})
	require.ErrorContains(t, err, "refusing an unwrapped restart")

	spec.Contract.StateDirs = nil
	_, missingStateDirs, err := openCodeSandboxRecord(&spec)
	require.NoError(t, err)
	_, err = openCodeRuntimeSandboxSpec(db.OpenCodeRuntime{
		Cwd:                   cwd,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		SandboxLaunchSpecJSON: missingStateDirs,
	})
	require.ErrorContains(t, err, "no mutable state directories")

	privateRoot, _, err := tclcommon.PrepareSpawnAttachmentsPrivateDir("spwn-opencode")
	require.NoError(t, err)
	agentID := db.NewAgentID()
	allocation, err := allocatePrivateOpenCodeState(agentID)
	require.NoError(t, err)
	freshSpec, err := openCodeTclaudeLayerLaunchSpec(
		string(sandboxpolicy.ImplementationTclaudeLayer),
		cwd,
		nil,
		nil,
		agentID,
		"spwn-opencode",
	)
	require.NoError(t, err)
	require.NotNil(t, freshSpec)
	require.Equal(t, []session.TclaudeLayerPrivateWriteDir{
		{Parent: filepath.Dir(allocation.StateRoot), Current: allocation.StateRoot},
		{Parent: tclcommon.SpawnAttachmentsPrivateBase(), Current: privateRoot},
	}, freshSpec.Contract.PrivateWriteDirs,
		"the persisted server spec must carry the tool executor's attachment root")
	freshSpec.Contract.ReadOnlyStateDirs = nil
	freshSpec.Contract.ReadOnlyBinds = nil
	_, missingReadOnlyState, err := openCodeSandboxRecord(freshSpec)
	require.NoError(t, err)
	_, err = openCodeRuntimeSandboxSpec(db.OpenCodeRuntime{
		Cwd:                   cwd,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		SandboxLaunchSpecJSON: missingReadOnlyState,
	})
	require.ErrorContains(t, err, "does not protect its executable state")
}

func TestOpenCodeServerEnvironmentAppliesFrozenExecutorProfile(t *testing.T) {
	spec := &session.TclaudeLayerLaunchSpec{
		Effective: sandboxpolicy.EffectiveProfile{
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: "PROFILE_VALUE", Value: "frozen"},
				{Name: "GOCACHE", Value: "/tmp/agent-cache"},
			},
		},
	}
	env := openCodeServerEnvironment([]string{
		"PATH=/usr/bin",
		"PROFILE_VALUE=ambient",
	}, spec)
	assert.Equal(t, "frozen", lastOpenCodeEnvironmentValue(env, "PROFILE_VALUE"))
	assert.Equal(t, "/tmp/agent-cache", lastOpenCodeEnvironmentValue(env, "GOCACHE"))
	assert.Equal(t, "/usr/bin", lastOpenCodeEnvironmentValue(env, "PATH"))
}

func lastOpenCodeEnvironmentValue(environment []string, name string) string {
	prefix := name + "="
	var value string
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			value = strings.TrimPrefix(entry, prefix)
		}
	}
	return value
}

func TestReconcileOpenCodeRuntimeNeverFallsBackFromMissingWrappedSpec(t *testing.T) {
	setupTestDB(t)
	previousResolve := resolveOpenCodeTclaudeLayer
	t.Cleanup(func() { resolveOpenCodeTclaudeLayer = previousResolve })
	resolveOpenCodeTclaudeLayer = func(
		sandboxpolicy.NetworkPosture,
	) (string, harness.LaunchOSSandbox, error) {
		t.Fatal("a wrapped runtime without its launch spec must refuse before any restart")
		return "", harness.LaunchOSSandbox{}, nil
	}
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID:             "spwn-wrapped-missing-spec",
		ConvID:                "ses-wrapped-missing-spec",
		ServerURL:             "http://127.0.0.1:1",
		Password:              "private",
		Cwd:                   "/tmp/project",
		PID:                   99_999_999,
		PermissionJSON:        openCodeTestPermissionJSON,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
	}))

	assert.False(t, reconcileOpenCodeRuntime("spwn-wrapped-missing-spec"))
}

func TestOpenCodeServeExecWrapsAuthoritativeServer(t *testing.T) {
	previousResolve := resolveOpenCodeTclaudeLayer
	previousWrap := wrapOpenCodeTclaudeLayer
	t.Cleanup(func() {
		resolveOpenCodeTclaudeLayer = previousResolve
		wrapOpenCodeTclaudeLayer = previousWrap
	})
	resolveOpenCodeTclaudeLayer = func(
		posture sandboxpolicy.NetworkPosture,
	) (string, harness.LaunchOSSandbox, error) {
		require.Equal(t, sandboxpolicy.NetworkHostOpen, posture)
		return "/usr/bin/bwrap", harness.LaunchOSSandbox{}, nil
	}
	var capturedCommand string
	wrapOpenCodeTclaudeLayer = func(
		binary string,
		spec session.TclaudeLayerLaunchSpec,
		command string,
	) (string, error) {
		assert.Equal(t, "/usr/bin/bwrap", binary)
		assert.Equal(t, session.TclaudeLayerLaunchSpecVersion, spec.Version)
		capturedCommand = command
		return "wrapped-opencode-server", nil
	}
	spec := &session.TclaudeLayerLaunchSpec{
		Version: session.TclaudeLayerLaunchSpecVersion,
		Effective: sandboxpolicy.EffectiveProfile{
			NetworkAccess: sandboxpolicy.NetworkAccessInherit,
		},
	}

	command, args, err := openCodeServeExec("/tmp/open code", "43210", spec)
	require.NoError(t, err)
	assert.Equal(t, "sh", command)
	assert.Equal(t, []string{"-c", "exec wrapped-opencode-server"}, args)
	assert.Contains(t, capturedCommand, "'/tmp/open code' serve")
	assert.Contains(t, capturedCommand, "--hostname 127.0.0.1")
	assert.Contains(t, capturedCommand, "--port 43210")
}

func TestOpenCodeServeExecRefusesNonHostOpenBeforeResolve(t *testing.T) {
	previousResolve := resolveOpenCodeTclaudeLayer
	t.Cleanup(func() { resolveOpenCodeTclaudeLayer = previousResolve })
	resolveOpenCodeTclaudeLayer = func(
		sandboxpolicy.NetworkPosture,
	) (string, harness.LaunchOSSandbox, error) {
		t.Fatal("host capability must not be probed for an unsupported OpenCode posture")
		return "", harness.LaunchOSSandbox{}, nil
	}
	spec := &session.TclaudeLayerLaunchSpec{
		Version: session.TclaudeLayerLaunchSpecVersion,
		Effective: sandboxpolicy.EffectiveProfile{
			NetworkAccess: sandboxpolicy.NetworkAccessNone,
		},
	}

	_, _, err := openCodeServeExec("/usr/bin/opencode", "43210", spec)
	require.ErrorContains(t, err, "host-open loopback control plane")
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
