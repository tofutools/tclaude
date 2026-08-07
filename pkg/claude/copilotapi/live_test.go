package copilotapi

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
func startLiveServer(t *testing.T) string {
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
			return address
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("copilot never listened on %s; logs in %s", address, logs)
	return ""
}

func liveClient(t *testing.T) *Client {
	t.Helper()
	address := startLiveServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := DialRetry(ctx, address, &Options{SubscriptionBuffer: 1024})
	if err != nil {
		t.Fatalf("connect to %s: %v", address, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
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

func TestLiveSessionsOpenReportsMissingSessionAsSuccess(t *testing.T) {
	// sessions.open is the method api.schema.json documents for adopting a
	// session, and it is a trap: against an unknown session it answers
	// {"status":"not_found"} as a successful result. Code that trusts it
	// proceeds against a session that does not exist.
	client := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result struct {
		Status string `json:"status"`
	}
	if err := client.Call(ctx, "sessions.open", map[string]string{"sessionId": NewSessionID()}, &result); err != nil {
		t.Fatalf("sessions.open returned an error; the trap may be fixed upstream: %v", err)
	}
	if result.Status != "not_found" {
		t.Errorf("status = %q, want %q", result.Status, "not_found")
	}
}

func TestLiveMultipleClientsShareOneServer(t *testing.T) {
	// Consumers are expected to split roles across connections — one driving
	// prompts, another consuming events — so this pins down that a second
	// connection sees the first one's session and can drive it.
	address := startLiveServer(t)
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
