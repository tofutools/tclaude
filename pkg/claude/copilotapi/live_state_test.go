package copilotapi

import (
	"context"
	"os"
	"testing"
	"time"
)

// Live pins for the point-in-time state reads.
//
// These are here rather than in a unit test because the thing being pinned is
// not the decode — that is covered against recorded payloads in state_test.go —
// but the SEMANTICS. "isProcessing is true for exactly as long as a turn is
// running" and "the context breakdown's parts sum to its total" are claims
// about a server, and a fake that answers them by construction proves nothing.
//
//	TCLAUDE_COPILOT_LIVE=1 TCLAUDE_COPILOT_LIVE_SEND=1 \
//	  go test ./pkg/claude/copilotapi/ -run TestLiveSessionState -v

func TestLiveSessionStateTracksATurn(t *testing.T) {
	if os.Getenv("TCLAUDE_COPILOT_LIVE_SEND") != "1" {
		t.Skip("set TCLAUDE_COPILOT_LIVE_SEND=1 to send a real prompt (consumes Copilot quota)")
	}
	client := liveClient(t)
	subscription := client.Subscribe()
	t.Cleanup(subscription.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	info, err := client.CreateSession(ctx, CreateSessionParams{
		WorkingDirectory: t.TempDir(), ClientName: "tclaude-copilotapi-test", Streaming: true,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := client.SetForegroundSession(ctx, info.SessionID); err != nil {
		t.Fatalf("SetForegroundSession: %v", err)
	}

	// Before the turn. A session that has been created and foregrounded but
	// never sent anything is not processing, and nothing is waiting on a human.
	processing, err := client.IsProcessing(ctx, info.SessionID)
	if err != nil {
		t.Fatalf("IsProcessing: %v", err)
	}
	if processing {
		t.Error("a session that has run nothing reports itself as processing")
	}
	pending, err := client.PendingPermissionRequests(ctx, info.SessionID)
	if err != nil {
		t.Fatalf("PendingPermissionRequests: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending permissions = %d before any turn", len(pending))
	}

	if _, err := client.Send(ctx, SendParams{
		SessionID: info.SessionID,
		Prompt:    "Run the shell command `sleep 5; echo done` and report its exact output.",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// During the turn. Sampled a few times rather than once, because a single
	// sample cannot tell "true while the turn runs" apart from "true once".
	deadline := time.Now().Add(30 * time.Second)
	sawProcessing := false
	for time.Now().Before(deadline) {
		processing, err := client.IsProcessing(ctx, info.SessionID)
		if err != nil {
			t.Fatalf("IsProcessing: %v", err)
		}
		if processing {
			sawProcessing = true
			activity, err := client.Activity(ctx, info.SessionID)
			if err != nil {
				t.Fatalf("Activity: %v", err)
			}
			if !activity.HasActiveWork {
				t.Error("activity reports no active work while the session is processing")
			}
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !sawProcessing {
		t.Fatal("the session never reported itself as processing during a turn")
	}

	// The turn ends on the event stream, and the read must agree with it.
	// `session.idle` is ephemeral, which is exactly why the read exists — but
	// the two describing the same moment is the contract worth pinning.
	waitForLiveIdle(ctx, t, subscription, info.SessionID)
	processing, err = client.IsProcessing(ctx, info.SessionID)
	if err != nil {
		t.Fatalf("IsProcessing: %v", err)
	}
	if processing {
		t.Error("the session still reports processing after session.idle")
	}

	// The context breakdown, checked as arithmetic rather than as field
	// presence. mcpToolsTokens is a SUBSET of toolDefinitionsTokens, so the
	// identity below is the one that holds — and summing all four instead is
	// the mistake it exists to catch.
	contextInfo, err := client.ContextInfo(ctx, ContextInfoParams{SessionID: info.SessionID})
	if err != nil {
		t.Fatalf("ContextInfo: %v", err)
	}
	if contextInfo == nil {
		t.Fatal("ContextInfo is still null after a completed turn")
	}
	parts := contextInfo.SystemTokens + contextInfo.ConversationTokens +
		contextInfo.ToolDefinitionTokens
	if parts != contextInfo.TotalTokens {
		t.Errorf("systemTokens + conversationTokens + toolDefinitionsTokens = %d, "+
			"but totalTokens = %d — the breakdown's parts no longer sum, so a meter "+
			"built on them is now wrong: %+v", parts, contextInfo.TotalTokens, *contextInfo)
	}
	if contextInfo.MCPToolsTokens > contextInfo.ToolDefinitionTokens {
		t.Errorf("mcpToolsTokens (%d) exceeds toolDefinitionsTokens (%d), so it is no "+
			"longer a subset of it", contextInfo.MCPToolsTokens, contextInfo.ToolDefinitionTokens)
	}
	if contextInfo.PromptTokenLimit <= 0 {
		t.Errorf("no promptTokenLimit: the API drive's whole context-meter improvement "+
			"is that this is a reported number rather than an assumed one: %+v", *contextInfo)
	}

	// The model is read from usage, never from the context breakdown, which was
	// measured naming a different model than the turn ran on under auto mode.
	metrics, err := client.UsageMetrics(ctx, info.SessionID)
	if err != nil {
		t.Fatalf("UsageMetrics: %v", err)
	}
	if metrics.CurrentModel == "" {
		t.Error("usage reports no current model after a completed turn")
	}
	var outputTokens int
	for _, model := range metrics.ModelMetrics {
		outputTokens += model.Usage.OutputTokens
	}
	if outputTokens == 0 {
		t.Error("a completed turn reported zero output tokens across every model; " +
			"the nested modelMetrics shape decodes as zeros when flattened")
	}
}

// waitForLiveIdle blocks until the session reports itself idle on the stream.
func waitForLiveIdle(
	ctx context.Context, t *testing.T, subscription *Subscription, sessionID string,
) {
	t.Helper()
	deadline := time.After(3 * time.Minute)
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
			// Only the ROOT agent's idle ends the turn. A sub-agent's carries an
			// agentId, and taking it would report the agent done mid-turn.
			if event.SessionID == sessionID && event.Event.Type == "session.idle" &&
				event.Event.AgentID == "" {
				return
			}
		case <-deadline:
			t.Fatal("turn never reached session.idle")
		case <-ctx.Done():
			t.Fatalf("gave up waiting for idle: %v", ctx.Err())
		}
	}
}
