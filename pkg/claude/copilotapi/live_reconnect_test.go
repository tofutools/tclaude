package copilotapi

import (
	"context"
	"testing"
	"time"
)

// The contract agentd's reconnect rests on, pinned against the real server.
//
//	TCLAUDE_COPILOT_LIVE=1 go test ./pkg/claude/copilotapi/ -run TestLiveReconnect -v
//
// tclaude holds Copilot API handles in process memory, so an agentd restart
// loses every one of them while the panes keep running. What makes recovery
// possible — and makes it possible WITHOUT any call that could open, reset or
// move a session — is that the server's session registry is process-global
// rather than per-connection: a second connection can drive a session the first
// one opened, and closing the first disposes nothing.
//
// That is a property of Copilot's server, not of anything tclaude controls. If
// it ever changes, the reconnect stops working in a way no unit test could see
// (agentd's fake server is a fake precisely because it does what we tell it),
// and the symptom in production would be agents that go quiet after a restart
// rather than an error anywhere. Hence this test.
//
// Costs no model quota: it never sends a prompt.
func TestLiveReconnectDrivesASessionOpenedByAClosedConnection(t *testing.T) {
	address, _ := startLiveServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	first, err := DialRetry(ctx, address, nil)
	if err != nil {
		t.Fatalf("dial the first connection: %v", err)
	}

	// The pane's own startup session, captured BEFORE we create ours. It is the
	// control for every positive below, and it has to be captured here because
	// foregrounding our own session is what displaces it.
	startup, err := first.GetForegroundSession(ctx)
	if err != nil {
		t.Fatalf("GetForegroundSession: %v", err)
	}
	if startup.SessionID == "" {
		t.Fatal("no startup session to use as a control")
	}

	sessionID := NewSessionID()
	if _, err := first.CreateSession(ctx, CreateSessionParams{
		SessionID: sessionID, ClientName: "tclaude-live-reconnect", Streaming: true,
	}); err != nil {
		t.Fatalf("create the session the reconnect will have to find: %v", err)
	}
	if err := first.SetForegroundSession(ctx, sessionID); err != nil {
		t.Fatalf("foreground it: %v", err)
	}
	if _, err := first.IsProcessing(ctx, sessionID); err != nil {
		t.Fatalf("precondition: the session must be drivable on the connection that "+
			"created it, or nothing below means anything: %v", err)
	}

	// The restart. Not a graceful hand-off — the connection that owns the
	// session simply goes away, which is what happens when agentd dies.
	if err := first.Close(); err != nil {
		t.Fatalf("close the first connection: %v", err)
	}

	second, err := DialRetry(ctx, address, nil)
	if err != nil {
		t.Fatalf("dial the second connection: %v", err)
	}
	defer func() { _ = second.Close() }()

	// THE CONTRACT. No session.create, no session.resume, no setForeground —
	// just a read, on a connection that has done nothing else. agentd's
	// reconnect is exactly this call and nothing more.
	if _, err := second.IsProcessing(ctx, sessionID); err != nil {
		t.Fatalf("a second connection could not drive the session the first one opened: "+
			"%v.\n\nagentd's reconnect (copilot_api_reconnect.go) is built on this being "+
			"possible with NO opening call. If Copilot has made the session registry "+
			"per-connection, or now disposes a connection's sessions when it closes, then "+
			"re-establishing a handle needs an opening call again — and the only candidate, "+
			"`session.resume`, mutates. That decision has to be re-made, not patched.", err)
	}

	// The control, and it is the half that makes the line above an answer rather
	// than a server that says yes to everything. The pane's own startup session
	// is NOT in the registry and must be refused.
	if _, err := second.IsProcessing(ctx, startup.SessionID); err == nil {
		t.Fatal("the pane's own startup session answered the drivability probe. That " +
			"probe is how the reconnect tells 'the server still holds this conversation' " +
			"from 'it does not' — if it answers for a session that is not in the registry, " +
			"it is not distinguishing anything and the reconnect would adopt handles that " +
			"cannot drive.")
	} else if !IsSessionNotFound(err) {
		t.Fatalf("the control failed for the wrong reason (%v); it must be a "+
			"session-not-found, or this is measuring a broken connection rather than "+
			"registry membership", err)
	}
}
