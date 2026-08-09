package codexappserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/codexappserver"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func startClient(t *testing.T, opts *codexappserver.Options) (*codexappserver.Client, *testharness.CodexAppServerSim) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "codexappserver-test-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	sim, err := testharness.StartCodexAppServerSim(filepath.Join(dir, "app.sock"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sim.Close()) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := codexappserver.Dial(ctx, sim.SocketPath(), opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client, sim
}

func nextMessage(t *testing.T, sim *testharness.CodexAppServerSim) testharness.CodexAppServerMessage {
	t.Helper()
	select {
	case message := <-sim.Messages():
		return message
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for fake app-server message")
		return testharness.CodexAppServerMessage{}
	}
}

func drainHandshake(t *testing.T, sim *testharness.CodexAppServerSim) (testharness.CodexAppServerMessage, testharness.CodexAppServerMessage) {
	t.Helper()
	initialize := nextMessage(t, sim)
	initialized := nextMessage(t, sim)
	require.Equal(t, codexappserver.MethodInitialize, initialize.Method)
	require.Equal(t, codexappserver.MethodInitialized, initialized.Method)
	return initialize, initialized
}

func TestDialUsesTruthfulStableHandshake(t *testing.T) {
	client, sim := startClient(t, &codexappserver.Options{ClientVersion: "v-test", CodexVersion: "codex-cli 0.147.0"})
	initialize, initialized := drainHandshake(t, sim)

	var params map[string]any
	require.NoError(t, json.Unmarshal(initialize.Params, &params))
	assert.Equal(t, "tclaude", params["clientInfo"].(map[string]any)["name"])
	assert.Equal(t, "v-test", params["clientInfo"].(map[string]any)["version"])
	assert.NotContains(t, params, "capabilities", "M1 must not opt into experimentalApi")
	assert.JSONEq(t, `{}`, string(initialized.Params))
	assert.Equal(t, "0.147.0", client.CodexVersion())
	assert.Equal(t, "codex_app_server/0.147.0", client.InitializeResult().UserAgent)
}

func TestConcurrentCallsCorrelateAcrossInterleavedNotification(t *testing.T) {
	client, sim := startClient(t, nil)
	drainHandshake(t, sim)

	type result struct {
		Value string `json:"value"`
	}
	values := make([]string, 8)
	errs := make([]error, len(values))
	var wg sync.WaitGroup
	for i := range values {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			var reply result
			errs[index] = client.Call(context.Background(), fmt.Sprintf("test/%d", index), map[string]int{"index": index}, &reply)
			values[index] = reply.Value
		}(i)
	}

	requests := make([]testharness.CodexAppServerMessage, len(values))
	for i := range requests {
		requests[i] = nextMessage(t, sim)
	}
	require.NoError(t, sim.SendNotification("thread/status/changed", map[string]string{"threadId": "thread-1"}))
	for i := len(requests) - 1; i >= 0; i-- {
		var params struct {
			Index int `json:"index"`
		}
		require.NoError(t, json.Unmarshal(requests[i].Params, &params))
		require.NoError(t, sim.Reply(requests[i].ID, result{Value: fmt.Sprintf("reply-%d", params.Index)}))
	}
	wg.Wait()
	for i := range values {
		require.NoError(t, errs[i])
		assert.Equal(t, fmt.Sprintf("reply-%d", i), values[i])
	}
	select {
	case notification := <-client.Notifications():
		assert.Equal(t, "thread/status/changed", notification.Method)
	case <-time.After(3 * time.Second):
		t.Fatal("notification was not delivered while calls were in flight")
	}
}

func TestM1MethodsUseStableWireShapes(t *testing.T) {
	client, sim := startClient(t, nil)
	drainHandshake(t, sim)

	replyNext := func(method string, result any) testharness.CodexAppServerMessage {
		t.Helper()
		message := nextMessage(t, sim)
		require.Equal(t, method, message.Method)
		require.NoError(t, sim.Reply(message.ID, result))
		return message
	}

	loadedDone := make(chan error, 1)
	go func() {
		_, err := client.ListLoadedThreads(context.Background(), codexappserver.ThreadLoadedListParams{})
		loadedDone <- err
	}()
	replyNext(codexappserver.MethodThreadLoadedList, map[string]any{"data": []string{"thread-1"}, "extra": true})
	require.NoError(t, <-loadedDone)

	readDone := make(chan error, 1)
	go func() {
		_, err := client.ReadThread(context.Background(), codexappserver.ThreadReadParams{ThreadID: "thread-1", IncludeTurns: true})
		readDone <- err
	}()
	replyNext(codexappserver.MethodThreadRead, map[string]any{"thread": map[string]any{
		"id": "thread-1", "status": map[string]any{"type": "idle"}, "turns": []any{}, "futureField": 1,
	}})
	require.NoError(t, <-readDone)

	forkDone := make(chan error, 1)
	go func() {
		cwd, last := "/tmp/fork", "turn-1"
		_, err := client.ForkThread(context.Background(), codexappserver.ThreadForkParams{
			ThreadID: "thread-1", Cwd: &cwd, LastTurnID: &last,
		})
		forkDone <- err
	}()
	fork := replyNext(codexappserver.MethodThreadFork, map[string]any{"thread": map[string]any{
		"id": "thread-2", "status": map[string]any{"type": "idle"}, "turns": []any{},
	}})
	assert.JSONEq(t, `{"threadId":"thread-1","cwd":"/tmp/fork","lastTurnId":"turn-1"}`, string(fork.Params))
	require.NoError(t, <-forkDone)

	startDone := make(chan error, 1)
	go func() {
		_, err := client.StartTurn(context.Background(), codexappserver.TurnStartParams{
			ThreadID: "thread-1", Input: []codexappserver.UserInput{codexappserver.TextInput("hello")},
		})
		startDone <- err
	}()
	start := replyNext(codexappserver.MethodTurnStart, map[string]any{"turn": map[string]any{
		"id": "turn-1", "status": "inProgress", "items": []any{}, "extra": "ignored",
	}})
	assert.JSONEq(t, `{"threadId":"thread-1","input":[{"type":"text","text":"hello"}]}`, string(start.Params))
	require.NoError(t, <-startDone)

	for method, call := range map[string]func() error{
		codexappserver.MethodThreadNameSet:      func() error { return client.SetThreadName(context.Background(), "thread-1", "name") },
		codexappserver.MethodThreadCompactStart: func() error { return client.StartCompaction(context.Background(), "thread-1") },
		codexappserver.MethodTurnInterrupt:      func() error { return client.InterruptTurn(context.Background(), "thread-1", "turn-1") },
	} {
		done := make(chan error, 1)
		go func() { done <- call() }()
		replyNext(method, map[string]any{})
		require.NoError(t, <-done)
	}

	steerDone := make(chan error, 1)
	go func() {
		_, err := client.SteerTurn(context.Background(), codexappserver.TurnSteerParams{
			ThreadID: "thread-1", ExpectedTurnID: "turn-1",
			Input: []codexappserver.UserInput{codexappserver.TextInput("now")},
		})
		steerDone <- err
	}()
	replyNext(codexappserver.MethodTurnSteer, map[string]string{"turnId": "turn-1"})
	require.NoError(t, <-steerDone)
}

func TestAccountRateLimitsReadUsesStableNonSubscribingRequest(t *testing.T) {
	client, sim := startClient(t, nil)
	drainHandshake(t, sim)
	type readResult struct {
		value codexappserver.AccountRateLimitsReadResult
		err   error
	}
	done := make(chan readResult, 1)
	go func() {
		result, err := client.ReadAccountRateLimits(context.Background())
		done <- readResult{value: result, err: err}
	}()
	message := nextMessage(t, sim)
	require.Equal(t, codexappserver.MethodAccountRateLimitsRead, message.Method)
	assert.JSONEq(t, `{}`, string(message.Params))
	require.NoError(t, sim.Reply(message.ID, map[string]any{
		"rateLimits": map[string]any{},
		"rateLimitsByLimitId": map[string]any{"codex": map[string]any{
			"limitId": "codex", "primary": map[string]any{
				"usedPercent": 7, "windowDurationMins": 300, "resetsAt": 123,
			},
		}},
	}))
	result := <-done
	require.NoError(t, result.err)
	require.NotNil(t, result.value.RateLimitsByLimitID["codex"].Primary)
	select {
	case extra := <-sim.Messages():
		t.Fatalf("rate-limit read unexpectedly subscribed or sent another request: %s", extra.Method)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestServerInteractionRequestIsSurfacedThenQuarantinesClient(t *testing.T) {
	methods := []string{
		codexappserver.MethodCommandApproval,
		codexappserver.MethodFileChangeApproval,
		codexappserver.MethodPermissionsApproval,
		codexappserver.MethodRequestUserInput,
		"future/serverRequest",
	}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			client, sim := startClient(t, nil)
			drainHandshake(t, sim)
			id, err := sim.SendRequest(method, map[string]string{"threadId": "thread-1"})
			require.NoError(t, err)

			select {
			case request := <-client.ServerRequests():
				assert.Equal(t, method, request.Method)
				assert.JSONEq(t, fmt.Sprintf("%d", id), string(request.ID))
				assert.Equal(t, method != "future/serverRequest", request.IsInteractionRequest())
			case <-time.After(3 * time.Second):
				t.Fatal("server request was not surfaced")
			}
			select {
			case <-client.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("unexpected server request did not quarantine the client")
			}
			assert.ErrorIs(t, client.Err(), codexappserver.ErrUnexpectedServerRequest)
		})
	}
}

func TestMalformedAndOversizeMessagesAreTerminal(t *testing.T) {
	t.Run("malformed JSON", func(t *testing.T) {
		client, sim := startClient(t, nil)
		drainHandshake(t, sim)
		require.NoError(t, sim.SendRaw(websocket.TextMessage, []byte(`{"method":`)))
		<-client.Done()
		assert.ErrorIs(t, client.Err(), codexappserver.ErrProtocol)
	})

	t.Run("bounded read", func(t *testing.T) {
		client, sim := startClient(t, &codexappserver.Options{MaxMessageBytes: 512})
		drainHandshake(t, sim)
		require.NoError(t, sim.SendNotification("large", map[string]string{"data": string(make([]byte, 1024))}))
		<-client.Done()
		assert.Error(t, client.Err())
	})
}

func TestTimeoutAfterWriteIsExplicitlyAmbiguous(t *testing.T) {
	client, sim := startClient(t, nil)
	drainHandshake(t, sim)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := client.Call(ctx, "test/no-reply", struct{}{}, nil)
	nextMessage(t, sim)
	var callErr *codexappserver.CallError
	require.ErrorAs(t, err, &callErr)
	assert.True(t, callErr.Ambiguous)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestPreCanceledCallIsNotTransmitted(t *testing.T) {
	client, sim := startClient(t, nil)
	drainHandshake(t, sim)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := client.SetThreadName(ctx, "thread-1", "must-not-send")
	var callErr *codexappserver.CallError
	require.ErrorAs(t, err, &callErr)
	assert.False(t, callErr.Ambiguous)
	assert.ErrorIs(t, err, context.Canceled)
	select {
	case message := <-sim.Messages():
		t.Fatalf("pre-canceled call was transmitted as %s", message.Method)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestDisconnectFailsInflightAndFutureCalls(t *testing.T) {
	client, sim := startClient(t, nil)
	drainHandshake(t, sim)
	done := make(chan error, 1)
	go func() { done <- client.Call(context.Background(), "test/disconnect", struct{}{}, nil) }()
	nextMessage(t, sim)
	require.NoError(t, sim.CloseClient())
	err := <-done
	var callErr *codexappserver.CallError
	require.ErrorAs(t, err, &callErr)
	assert.True(t, callErr.Ambiguous)
	assert.Error(t, client.Call(context.Background(), "test/dead", nil, nil))
}

func TestVersionCompatibilityRange(t *testing.T) {
	for _, version := range []string{"0.147.0", "0.147.1", "codex-cli 0.147.99"} {
		assert.NoError(t, codexappserver.CheckVersion(version), version)
	}
	for _, version := range []string{"0.146.9", "0.148.0", "1.147.0", "dev"} {
		assert.ErrorIs(t, codexappserver.CheckVersion(version), codexappserver.ErrUnsupportedVersion, version)
	}
}

func TestDialRejectsUnverifiedOrMismatchedServerVersion(t *testing.T) {
	for _, test := range []struct {
		name         string
		userAgent    string
		codexVersion string
	}{
		{name: "outside range", userAgent: "codex_app_server/0.148.0"},
		{name: "unidentified", userAgent: "future-server"},
		{name: "launch mismatch", userAgent: "codex_app_server/0.147.1", codexVersion: "0.147.0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("/tmp", "codexappserver-version-")
			require.NoError(t, err)
			defer os.RemoveAll(dir)
			sim, err := testharness.StartCodexAppServerSim(filepath.Join(dir, "app.sock"))
			require.NoError(t, err)
			defer sim.Close()
			sim.InitializeResult.UserAgent = test.userAgent
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err = codexappserver.Dial(ctx, sim.SocketPath(), &codexappserver.Options{CodexVersion: test.codexVersion})
			assert.ErrorIs(t, err, codexappserver.ErrUnsupportedVersion)
		})
	}
}

func TestUnknownNotificationIsToleratedAndQueueIsBounded(t *testing.T) {
	client, sim := startClient(t, &codexappserver.Options{NotificationBuffer: 1})
	drainHandshake(t, sim)
	require.NoError(t, sim.SendNotification("future/added", map[string]bool{"unknown": true}))
	require.NoError(t, sim.SendNotification("future/overrun", struct{}{}))
	select {
	case <-client.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("notification overrun did not terminate the connection")
	}
	assert.True(t, errors.Is(client.Err(), codexappserver.ErrNotificationOverrun))
}
