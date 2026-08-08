package copilotapi

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// These tests drive a real `copilot --ui-server` process. They are opt-in
// because they need Copilot CLI installed and authenticated, which CI is not,
// and because the contract they pin down is only observable against the real
// server.
//
//	TCLAUDE_COPILOT_LIVE=1 go test ./pkg/claude/copilotapi/ -run TestLive -v
//
// Sending a prompt additionally consumes the operator's Copilot quota, so it
// sits behind a second switch:
//
//	TCLAUDE_COPILOT_LIVE=1 TCLAUDE_COPILOT_LIVE_SEND=1 go test ./pkg/claude/copilotapi/ -run TestLive -v
//
// Each run uses a throwaway COPILOT_HOME and log directory so it cannot
// disturb the operator's real profile.

func requireLive(t *testing.T) string {
	t.Helper()
	if os.Getenv("TCLAUDE_COPILOT_LIVE") != "1" {
		t.Skip("set TCLAUDE_COPILOT_LIVE=1 to run against a real copilot --ui-server")
	}
	binary, err := exec.LookPath("copilot")
	if err != nil {
		t.Skipf("copilot not on PATH: %v", err)
	}
	return binary
}

// freePort asks the kernel for an unused port and releases it. The gap before
// Copilot binds is a race in principle; in a test on a developer machine it is
// not worth more machinery than this.
func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}
	return port
}

// startLiveServer launches Copilot in TUI+server mode and returns its address.
//
// The PTY is not optional. Without a terminal the CLI takes a different
// startup branch and never mounts the TUI, so the embedded server never
// starts; the process simply exits having logged nothing about a listener.
func startLiveServer(t *testing.T) (string, *exec.Cmd) {
	t.Helper()
	binary := requireLive(t)
	port := freePort(t)
	root := t.TempDir()
	home := filepath.Join(root, "home")
	logs := filepath.Join(root, "logs")
	workdir := filepath.Join(root, "work")
	for _, dir := range []string{home, logs, workdir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	command := exec.Command(binary,
		"--ui-server",
		"--port", strconv.Itoa(port),
		"--allow-all-tools",
		"--log-dir", logs,
	)
	command.Dir = workdir
	command.Env = append(os.Environ(), "COPILOT_HOME="+home)

	terminal, err := pty.Start(command)
	if err != nil {
		t.Fatalf("start copilot under a pty: %v", err)
	}
	// Drain the TUI's screen output; a full pipe would wedge the process.
	go func() {
		buffer := make([]byte, 4096)
		for {
			if _, err := terminal.Read(buffer); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		_ = command.Wait()
		_ = terminal.Close()
	})

	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	// The listener comes up a few seconds after exec, behind auth and
	// workspace initialisation.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err == nil {
			_ = conn.Close()
			return address, command
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("copilot never listened on %s; logs in %s", address, logs)
	return "", nil
}

func liveClient(t *testing.T) *Client {
	t.Helper()
	client, _ := liveClientAndProcess(t)
	return client
}

// liveClientAndProcess is liveClient plus the copilot process itself, for the
// one contract that is about whether the PROCESS is still there.
func liveClientAndProcess(t *testing.T) (*Client, *exec.Cmd) {
	t.Helper()
	address, command := startLiveServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := DialRetry(ctx, address, &Options{SubscriptionBuffer: 1024})
	if err != nil {
		t.Fatalf("connect to %s: %v", address, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, command
}

func TestLiveHandshakeAndPing(t *testing.T) {
	client := liveClient(t)

	if got := client.ProtocolVersion(); got != SupportedProtocolVersion {
		t.Errorf("ProtocolVersion() = %d, want %d", got, SupportedProtocolVersion)
	}
	if client.ServerVersion() == "" {
		t.Error("ServerVersion() is empty")
	}
	t.Logf("copilot %s, protocol %d", client.ServerVersion(), client.ProtocolVersion())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := client.Ping(ctx, "tclaude")
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if result.Message != "pong: tclaude" {
		t.Errorf("Message = %q, want %q", result.Message, "pong: tclaude")
	}
}

func TestLiveTUISessionIsNotDrivable(t *testing.T) {
	// This is the trap the package docs describe: the pane's startup session
	// is reported by getForeground but rejects every session.* call. If this
	// ever stops being true, the bootstrap advice in the docs is stale.
	client := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	info, err := client.GetForegroundSession(ctx)
	if err != nil {
		t.Fatalf("GetForegroundSession: %v", err)
	}
	if info.SessionID == "" {
		t.Fatal("GetForegroundSession returned no session ID")
	}

	_, err = client.UsageMetrics(ctx, info.SessionID)
	if err == nil {
		t.Fatal("the TUI's own session accepted session.usage.getMetrics; the docs are now wrong")
	}
	if !IsSessionNotFound(err) {
		t.Fatalf("err = %v, want a session-not-found error", err)
	}
}

// openResult is the subset of the sessions.open reply the traps turn on.
type openResult struct {
	Status    string `json:"status"`
	SessionID string `json:"sessionId"`
}

// assertUndrivable proves a session ID cannot actually be driven, which is the
// point of every sessions.open trap: the open reports success and the failure
// only appears on a later, unrelated call.
func assertUndrivable(ctx context.Context, t *testing.T, client *Client, sessionID, what string) {
	t.Helper()
	err := client.SetSessionName(ctx, sessionID, "should not work")
	if err == nil {
		t.Fatalf("%s produced a drivable session; the docs are now wrong", what)
	}
	if !IsSessionNotFound(err) {
		t.Fatalf("%s: name.set err = %v, want a session-not-found error", what, err)
	}
	// setForeground refuses in-band rather than as a JSON-RPC error.
	if err := client.SetForegroundSession(ctx, sessionID); err == nil {
		t.Fatalf("%s: setForeground reported success for an undrivable session", what)
	}
}

func TestLiveSessionsOpenCreatesUndrivableSessions(t *testing.T) {
	// sessions.open is the session-opening method api.schema.json documents,
	// and every path through it fails in a way that reads as success. The
	// hazard is not a missing create path: `kind: "create"` exists, is fully
	// specified in the schema, and genuinely creates a session — one that is
	// never registered with the RPC session registry and so cannot be driven.
	client := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	t.Run("create reports created but is undrivable", func(t *testing.T) {
		var result openResult
		params := map[string]any{
			"kind":    "create",
			"options": map[string]any{"workingDirectory": t.TempDir()},
		}
		if err := client.Call(ctx, "sessions.open", params, &result); err != nil {
			t.Fatalf("sessions.open create errored; the trap may be fixed upstream: %v", err)
		}
		if result.Status != "created" {
			t.Fatalf("status = %q, want %q", result.Status, "created")
		}
		if result.SessionID == "" {
			t.Fatal("sessions.open create returned no session ID")
		}
		assertUndrivable(ctx, t, client, result.SessionID, "sessions.open create")
	})

	t.Run("attach reports resumed but is undrivable", func(t *testing.T) {
		foreground, err := client.GetForegroundSession(ctx)
		if err != nil {
			t.Fatalf("GetForegroundSession: %v", err)
		}
		var result openResult
		params := map[string]any{"kind": "attach", "sessionId": foreground.SessionID}
		if err := client.Call(ctx, "sessions.open", params, &result); err != nil {
			t.Fatalf("sessions.open attach errored; the trap may be fixed upstream: %v", err)
		}
		if result.Status != "resumed" {
			t.Fatalf("status = %q, want %q", result.Status, "resumed")
		}
		assertUndrivable(ctx, t, client, foreground.SessionID, "sessions.open attach")
	})

	t.Run("unknown session reports not_found as success", func(t *testing.T) {
		// Documented behaviour — not_found is a SessionsOpenStatus value — so
		// the hazard is a caller checking only for transport and JSON-RPC
		// errors and sailing past the status field.
		var result openResult
		params := map[string]any{"kind": "attach", "sessionId": NewSessionID()}
		if err := client.Call(ctx, "sessions.open", params, &result); err != nil {
			t.Fatalf("sessions.open errored; the trap may be fixed upstream: %v", err)
		}
		if result.Status != "not_found" {
			t.Errorf("status = %q, want %q", result.Status, "not_found")
		}
	})
}

func TestLiveMultipleClientsShareOneServer(t *testing.T) {
	// Consumers are expected to split roles across connections — one driving
	// prompts, another consuming events — so this pins down that a second
	// connection sees the first one's session and can drive it.
	address, _ := startLiveServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	driver, err := DialRetry(ctx, address, nil)
	if err != nil {
		t.Fatalf("connect driver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })

	observer, err := DialRetry(ctx, address, nil)
	if err != nil {
		t.Fatalf("connect observer: %v", err)
	}
	t.Cleanup(func() { _ = observer.Close() })

	// Subscribe before the session exists so the creation event cannot be
	// missed.
	subscription := observer.Subscribe()
	t.Cleanup(subscription.Close)

	info, err := driver.CreateSession(ctx, CreateSessionParams{
		WorkingDirectory: t.TempDir(),
		ClientName:       "tclaude-copilotapi-driver",
	})
	if err != nil {
		t.Fatalf("CreateSession on the driver: %v", err)
	}

	// The observer must see the driver's session on its own event stream.
	deadline := time.After(30 * time.Second)
	for seen := false; !seen; {
		select {
		case notification, ok := <-subscription.C():
			if !ok {
				t.Fatalf("observer subscription ended early: %v", subscription.Err())
			}
			if notification.Method != MethodSessionLifecycle {
				continue
			}
			lifecycle, err := notification.Lifecycle()
			if err != nil {
				t.Fatalf("decode lifecycle: %v", err)
			}
			if lifecycle.SessionID == info.SessionID && lifecycle.Type == LifecycleSessionCreated {
				seen = true
			}
		case <-deadline:
			t.Fatal("the observer never saw the driver's session.created event")
		}
	}

	// ...and must be able to drive a session it did not create.
	if err := observer.SetSessionName(ctx, info.SessionID, "renamed by the observer"); err != nil {
		t.Fatalf("observer could not drive the driver's session: %v", err)
	}

	// One client closing must not disturb the other.
	_ = driver.Close()
	if _, err := observer.Ping(ctx, "still here"); err != nil {
		t.Errorf("observer broke when the driver disconnected: %v", err)
	}
}

func TestLiveSessionBootstrap(t *testing.T) {
	client := liveClient(t)
	subscription := client.Subscribe()
	t.Cleanup(subscription.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	workdir := t.TempDir()
	info, err := client.CreateSession(ctx, CreateSessionParams{
		WorkingDirectory: workdir,
		ClientName:       "tclaude-copilotapi-test",
		Streaming:        true,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if info.WorkspacePath == "" {
		t.Error("CreateSession returned no workspace path")
	}

	if err := client.SetForegroundSession(ctx, info.SessionID); err != nil {
		t.Fatalf("SetForegroundSession: %v", err)
	}
	if err := client.SetSessionName(ctx, info.SessionID, "tclaude copilotapi live test"); err != nil {
		t.Fatalf("SetSessionName: %v", err)
	}

	// A session that has not run a turn yet reports no context info. Both
	// outcomes are valid; what must not happen is an error.
	contextInfo, err := client.ContextInfo(ctx, ContextInfoParams{SessionID: info.SessionID})
	if err != nil {
		t.Fatalf("ContextInfo: %v", err)
	}
	if contextInfo != nil {
		t.Logf("context info: %d/%d tokens", contextInfo.TotalTokens, contextInfo.Limit)
	}

	metrics, err := client.UsageMetrics(ctx, info.SessionID)
	if err != nil {
		t.Fatalf("UsageMetrics: %v", err)
	}
	if metrics.SessionStartTime == "" {
		t.Error("UsageMetrics returned no session start time")
	}

	// Creating and foregrounding our session must be observable on the event
	// stream, since that is how consumers will track agent state.
	wantLifecycle := map[string]bool{
		LifecycleSessionCreated:    false,
		LifecycleSessionForeground: false,
	}
	deadline := time.After(30 * time.Second)
	for {
		remaining := 0
		for _, seen := range wantLifecycle {
			if !seen {
				remaining++
			}
		}
		if remaining == 0 {
			break
		}
		select {
		case notification, ok := <-subscription.C():
			if !ok {
				t.Fatalf("subscription ended early: %v", subscription.Err())
			}
			if notification.Method != MethodSessionLifecycle {
				continue
			}
			lifecycle, err := notification.Lifecycle()
			if err != nil {
				t.Fatalf("decode lifecycle: %v", err)
			}
			if lifecycle.SessionID == info.SessionID {
				if _, wanted := wantLifecycle[lifecycle.Type]; wanted {
					wantLifecycle[lifecycle.Type] = true
				}
			}
		case <-deadline:
			t.Fatalf("did not observe expected lifecycle events: %v", wantLifecycle)
		}
	}
}

func TestLiveSend(t *testing.T) {
	if os.Getenv("TCLAUDE_COPILOT_LIVE_SEND") != "1" {
		t.Skip("set TCLAUDE_COPILOT_LIVE_SEND=1 to send a real prompt (consumes Copilot quota)")
	}
	client := liveClient(t)
	subscription := client.Subscribe()
	t.Cleanup(subscription.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	info, err := client.CreateSession(ctx, CreateSessionParams{
		WorkingDirectory: t.TempDir(),
		ClientName:       "tclaude-copilotapi-test",
		Streaming:        true,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := client.SetForegroundSession(ctx, info.SessionID); err != nil {
		t.Fatalf("SetForegroundSession: %v", err)
	}

	messageID, err := client.Send(ctx, SendParams{
		SessionID: info.SessionID,
		Prompt:    "Reply with exactly the word: ack",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if messageID == "" {
		t.Error("Send returned no message ID")
	}

	// session.send is fire-and-forget; completion is observed on the stream.
	//
	// This budget stays inside ctx's, so a turn that never finishes fails here
	// with "never reached session.idle" rather than racing ctx and surfacing
	// as a context deadline from whichever call happened to be in flight.
	deadline := time.After(90 * time.Second)
	for {
		select {
		case notification, ok := <-subscription.C():
			if !ok {
				t.Fatalf("subscription ended early: %v", subscription.Err())
			}
			if notification.Method != MethodSessionEvent {
				continue
			}
			event, err := notification.SessionEvent()
			if err != nil {
				t.Fatalf("decode session event: %v", err)
			}
			if event.SessionID != info.SessionID {
				continue
			}
			t.Logf("event: %s", event.Event.Type)
			if event.Event.Type == "session.idle" {
				metrics, err := client.UsageMetrics(ctx, info.SessionID)
				if err != nil {
					t.Fatalf("UsageMetrics: %v", err)
				}
				if metrics.TotalUserRequests == 0 {
					t.Error("a completed turn reported no user requests")
				}
				encoded, _ := json.Marshal(metrics.ModelMetrics)
				t.Logf("model metrics: %s", encoded)
				// Decoding the metrics with the wrong field names still
				// succeeds and yields zeros, so a completed turn must be
				// shown to produce real token counts.
				var inputTokens, requests int
				for _, model := range metrics.ModelMetrics {
					inputTokens += model.Usage.InputTokens
					requests += model.Requests.Count
				}
				if inputTokens == 0 {
					t.Error("a completed turn reported zero input tokens across every model")
				}
				if requests == 0 {
					t.Error("a completed turn reported zero requests across every model")
				}

				// Context info is null before the first turn, so this is the
				// only point at which its field names can be checked against
				// a populated payload.
				contextInfo, err := client.ContextInfo(ctx, ContextInfoParams{SessionID: info.SessionID})
				if err != nil {
					t.Fatalf("ContextInfo: %v", err)
				}
				if contextInfo == nil {
					t.Fatal("ContextInfo is still null after a completed turn")
				}
				t.Logf("context info: %+v", *contextInfo)
				if contextInfo.TotalTokens == 0 || contextInfo.SystemTokens == 0 {
					t.Errorf("ContextInfo reported zero tokens: %+v", *contextInfo)
				}
				if contextInfo.PromptTokenLimit == 0 || contextInfo.Limit == 0 {
					t.Errorf("ContextInfo reported no limits: %+v", *contextInfo)
				}
				if contextInfo.ModelName == "" {
					t.Error("ContextInfo reported no model name")
				}
				return
			}
		case <-deadline:
			t.Fatal("turn never reached session.idle")
		}
	}
}

// liveDrivableSession is the bootstrap TestLiveSessionBootstrap pins, reduced
// to the two calls every later contract needs in front of it.
func liveDrivableSession(ctx context.Context, t *testing.T, client *Client) string {
	t.Helper()
	info, err := client.CreateSession(ctx, CreateSessionParams{
		WorkingDirectory: t.TempDir(),
		ClientName:       "tclaude-copilotapi-test",
		Streaming:        true,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := client.SetForegroundSession(ctx, info.SessionID); err != nil {
		t.Fatalf("SetForegroundSession: %v", err)
	}
	return info.SessionID
}

// TestLiveShutdownRPCsDoNotEndTheProcess is why tclaude's soft exit stays on
// tmux send-keys for an API-driven Copilot agent (TCL-1058).
//
// `session.shutdown` and `sessions.close` read like the typed replacement for
// `/exit`, and they are not: both succeed, and the copilot process is still
// running afterwards with its session still foregrounded. They end a SESSION,
// not the CLI. Routing soft exit through them would report a delivered exit for
// a pane that never dies, which every stop, retire and dashboard surface then
// reads as an agent that finished its work.
//
// `runtime.shutdown` — the one method that sounds like it would end the
// process — refuses outright in this mode.
//
// If this test ever starts failing because the process DOES exit, the soft-exit
// decision is worth revisiting; that is the entire reason it is written down
// here rather than only in a comment.
func TestLiveShutdownRPCsDoNotEndTheProcess(t *testing.T) {
	client, command := liveClientAndProcess(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	sessionID := liveDrivableSession(ctx, t, client)

	if !liveProcessAlive(command.Process.Pid) {
		t.Fatal("precondition: the copilot process must be running")
	}

	if err := client.Call(ctx, "session.shutdown",
		map[string]any{"sessionId": sessionID, "type": "routine"}, nil); err != nil {
		t.Fatalf("session.shutdown: %v", err)
	}
	if !liveProcessAlive(command.Process.Pid) {
		t.Fatal("session.shutdown ended the copilot process; soft exit could now use it")
	}
	// Still answering, which is the sharper half: the connection and the server
	// both outlive the session's own shutdown.
	if _, err := client.GetForegroundSession(ctx); err != nil {
		t.Errorf("the server stopped answering after session.shutdown: %v", err)
	}

	if err := client.Call(ctx, "sessions.close",
		map[string]any{"sessionId": sessionID}, nil); err != nil {
		t.Fatalf("sessions.close: %v", err)
	}
	if !liveProcessAlive(command.Process.Pid) {
		t.Fatal("sessions.close ended the copilot process; soft exit could now use it")
	}

	if err := client.Call(ctx, "runtime.shutdown", struct{}{}, nil); err == nil {
		t.Fatal("runtime.shutdown succeeded; this mode used to refuse it outright")
	}
	if liveProcessAlive(command.Process.Pid) {
		return
	}
	t.Fatal("runtime.shutdown ended the copilot process despite reporting a refusal")
}

// liveProcessAlive reads the kernel's own view rather than signalling, so a
// child this test has not reaped is not mistaken for a live process.
func liveProcessAlive(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	fields := strings.Fields(string(stat))
	// Field 3 is the state; Z is a zombie awaiting Wait().
	return len(fields) >= 3 && fields[2] != "Z"
}

// A session with no history refuses compaction with an ERROR rather than an
// empty success, and that refusal is the ordinary state of an agent that has
// barely started. A caller that cannot separate it from a real failure reports
// a broken channel that is not broken.
func TestLiveCompactRefusesASessionWithNoHistory(t *testing.T) {
	client := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	sessionID := liveDrivableSession(ctx, t, client)

	_, err := client.Compact(ctx, CompactParams{
		SessionID: sessionID, Trigger: CompactTriggerManual,
	})
	if err == nil {
		t.Fatal("compacting an empty session succeeded; IsNothingToCompact may be dead code now")
	}
	if !IsNothingToCompact(err) {
		t.Fatalf("err = %v, want the server's nothing-to-compact refusal", err)
	}
}

// queueSnapshot is the subset of session.queue.pendingItems the two send lanes
// are distinguished by.
type queueSnapshot struct {
	Items []struct {
		Kind        string `json:"kind"`
		DisplayText string `json:"displayText"`
	} `json:"items"`
	SteeringMessages []string `json:"steeringMessages"`
}

// TestLiveSendModesUseSeparateLanes settles the open question TCL-1058 carried:
// whether agent-to-agent delivery should use `immediate` or the default.
//
// The names oversell the difference. NEITHER mode interrupts the turn in
// flight: `enqueue` appends to `items`, `immediate` lands in the separate
// `steeringMessages` lane, and both run only once the current turn unwinds —
// `immediate` simply runs first. So `immediate` buys no promptness at all and
// costs the property that matters, which is that a message from another agent
// does not overtake what the human queued into the pane.
//
// Hence the default. See copilotapi.SendModeEnqueue and agentd's
// sendCopilotAPIMessage.
func TestLiveSendModesUseSeparateLanes(t *testing.T) {
	if os.Getenv("TCLAUDE_COPILOT_LIVE_SEND") != "1" {
		t.Skip("set TCLAUDE_COPILOT_LIVE_SEND=1 to send a real prompt (consumes Copilot quota)")
	}
	client := liveClient(t)
	subscription := client.Subscribe()
	t.Cleanup(subscription.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	sessionID := liveDrivableSession(ctx, t, client)

	// A turn long enough that the next two sends land while it is running.
	if _, err := client.Send(ctx, SendParams{
		SessionID: sessionID,
		Prompt: "Count slowly from 1 to 30, one number per line, with a short " +
			"sentence about each number.",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Waited for rather than slept past. The whole claim below is about what
	// happens to a message sent WHILE a turn is running, and a fixed sleep
	// against an unauthenticated or instantly-failing turn would leave the two
	// sends sitting in an idle session's queue — which looks identical and
	// proves nothing.
	awaitLiveTurnStart(ctx, t, subscription, sessionID)

	const queued = "QUEUED-MESSAGE: say the word banana."
	if _, err := client.Send(ctx, SendParams{
		SessionID: sessionID, Prompt: queued,
	}); err != nil {
		t.Fatalf("Send enqueue: %v", err)
	}
	if _, err := client.Send(ctx, SendParams{
		SessionID: sessionID,
		Prompt:    "IMMEDIATE-MESSAGE: say the word papaya.",
		Mode:      SendModeImmediate,
	}); err != nil {
		t.Fatalf("Send immediate: %v", err)
	}

	var snapshot queueSnapshot
	if err := client.Call(ctx, "session.queue.pendingItems",
		map[string]any{"sessionId": sessionID}, &snapshot); err != nil {
		t.Fatalf("session.queue.pendingItems: %v", err)
	}
	t.Logf("queue: %+v", snapshot)

	// Matched by kind and text rather than by position or count: the queue is
	// not ours alone. Copilot enqueues its own entries — a fresh profile puts a
	// `command` item (`/model auto`) in front of everything — so a consumer
	// reasoning about "the first item" or "how many items" is reading a lane it
	// only partly owns.
	queuedMessages := 0
	for _, item := range snapshot.Items {
		if item.Kind == "message" && item.DisplayText == queued {
			queuedMessages++
		}
	}
	if queuedMessages != 1 {
		t.Errorf("the default mode did not land in the queued lane as a message: %+v",
			snapshot.Items)
	}
	// The half that makes the decision: neither send interrupted the turn that
	// was proved to be in flight above. Both are still pending, `immediate`
	// included — so it is a queue-jump into a separate lane, not an
	// interjection, and choosing it for agent mail would buy nothing but a
	// reordering of the human's own input.
	if len(snapshot.SteeringMessages) != 1 {
		t.Errorf("immediate did not land in the steering lane, alive and unconsumed: %+v",
			snapshot.SteeringMessages)
	}
}

// awaitLiveTurnStart blocks until the session reports a turn actually running.
func awaitLiveTurnStart(
	ctx context.Context, t *testing.T, subscription *Subscription, sessionID string,
) {
	t.Helper()
	deadline := time.After(60 * time.Second)
	for {
		select {
		case notification, ok := <-subscription.C():
			if !ok {
				t.Fatalf("subscription ended early: %v", subscription.Err())
			}
			if notification.Method != MethodSessionEvent {
				continue
			}
			event, err := notification.SessionEvent()
			if err != nil {
				t.Fatalf("decode session event: %v", err)
			}
			if event.SessionID == sessionID && event.Event.Type == "assistant.turn_start" {
				return
			}
		case <-deadline:
			t.Fatal("no turn ever started; the send-lane contract cannot be measured " +
				"against an idle session (is this Copilot authenticated?)")
		case <-ctx.Done():
			t.Fatalf("waiting for a turn to start: %v", ctx.Err())
		}
	}
}
