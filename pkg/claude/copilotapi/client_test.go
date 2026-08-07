package copilotapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func dialTest(t *testing.T, server *fakeServer, opts *Options) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Dial(ctx, server.addr(), opts)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestDialRecordsServerIdentity(t *testing.T) {
	server := newFakeServer(t)
	client := dialTest(t, server, nil)

	if got := client.ProtocolVersion(); got != SupportedProtocolVersion {
		t.Errorf("ProtocolVersion() = %d, want %d", got, SupportedProtocolVersion)
	}
	if got := client.ServerVersion(); got != "1.0.78" {
		t.Errorf("ServerVersion() = %q, want %q", got, "1.0.78")
	}
	if client.ProtocolMismatch() {
		t.Error("ProtocolMismatch() = true for a matching server")
	}
	if seen := server.methodsSeen(); len(seen) != 1 || seen[0] != MethodConnect {
		t.Errorf("server saw %v, want just the handshake", seen)
	}
}

func TestDialRejectsProtocolMismatch(t *testing.T) {
	server := newFakeServer(t)
	server.handle(MethodConnect, func(json.RawMessage) (any, *Error) {
		return ConnectResult{OK: true, ProtocolVersion: SupportedProtocolVersion + 1, Version: "9.9.9"}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := Dial(ctx, server.addr(), nil)
	if !errors.Is(err, ErrProtocolVersion) {
		t.Fatalf("Dial err = %v, want ErrProtocolVersion", err)
	}
	// The failure has to name both versions, or an operator cannot tell which
	// side moved.
	if !strings.Contains(err.Error(), "9.9.9") {
		t.Errorf("error %q does not mention the server CLI version", err)
	}
}

func TestDialAllowsProtocolMismatchWhenOptedIn(t *testing.T) {
	server := newFakeServer(t)
	server.handle(MethodConnect, func(json.RawMessage) (any, *Error) {
		return ConnectResult{OK: true, ProtocolVersion: SupportedProtocolVersion + 1, Version: "9.9.9"}, nil
	})
	client := dialTest(t, server, &Options{AllowProtocolMismatch: true})

	if !client.ProtocolMismatch() {
		t.Error("ProtocolMismatch() = false, want true")
	}
	if got := client.ProtocolVersion(); got != SupportedProtocolVersion+1 {
		t.Errorf("ProtocolVersion() = %d, want %d", got, SupportedProtocolVersion+1)
	}
}

func TestDialRejectsHandshakeNotOK(t *testing.T) {
	server := newFakeServer(t)
	server.handle(MethodConnect, func(json.RawMessage) (any, *Error) {
		return ConnectResult{OK: false, ProtocolVersion: SupportedProtocolVersion}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Dial(ctx, server.addr(), nil); err == nil {
		t.Fatal("Dial succeeded against a server that did not report ok")
	}
}

func TestCallsCorrelateOutOfOrder(t *testing.T) {
	server := newFakeServer(t)
	// Replies come back in the reverse of the order they were requested, so a
	// client that assumed FIFO would hand callers each other's results.
	server.handle("echo", func(params json.RawMessage) (any, *Error) {
		var request struct {
			N     int `json:"n"`
			Delay int `json:"delayMs"`
		}
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, &Error{Code: CodeInvalidParams, Message: err.Error()}
		}
		time.Sleep(time.Duration(request.Delay) * time.Millisecond)
		return map[string]int{"n": request.N}, nil
	})
	client := dialTest(t, server, nil)

	const calls = 25
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for i := range calls {
		wg.Go(func() {
			var result struct {
				N int `json:"n"`
			}
			params := map[string]int{"n": i, "delayMs": (calls - i) * 4}
			if err := client.Call(context.Background(), "echo", params, &result); err != nil {
				errs <- err
				return
			}
			if result.N != i {
				errs <- fmt.Errorf("call %d got reply for %d", i, result.N)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestCallReturnsServerError(t *testing.T) {
	server := newFakeServer(t)
	server.handle(MethodSessionUsage, func(json.RawMessage) (any, *Error) {
		return nil, &Error{
			Code:    CodeInternalError,
			Message: "Request session.usage.getMetrics failed with message: Session not found for sessionId: abc",
		}
	})
	client := dialTest(t, server, nil)

	_, err := client.UsageMetrics(context.Background(), "abc")
	var rpcErr *Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("err = %v, want an *Error", err)
	}
	if rpcErr.Code != CodeInternalError {
		t.Errorf("Code = %d, want %d", rpcErr.Code, CodeInternalError)
	}
	if !IsSessionNotFound(err) {
		t.Error("IsSessionNotFound() = false for the server's session-not-found error")
	}
}

func TestIsSessionNotFoundIgnoresOtherErrors(t *testing.T) {
	if IsSessionNotFound(errors.New("boom")) {
		t.Error("IsSessionNotFound() = true for a plain error")
	}
	if IsSessionNotFound(&Error{Code: CodeInternalError, Message: "missing parameter 'name'"}) {
		t.Error("IsSessionNotFound() = true for an unrelated rpc error")
	}
}

func TestCallHonoursContextCancellation(t *testing.T) {
	server := newFakeServer(t)
	release := make(chan struct{})
	server.handle("slow", func(json.RawMessage) (any, *Error) {
		<-release
		return map[string]string{}, nil
	})
	t.Cleanup(func() { close(release) })
	client := dialTest(t, server, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := client.Call(ctx, "slow", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}

	// The abandoned call must not poison later ones on the same connection:
	// its pending entry has to be gone, and the reply that eventually arrives
	// for it must not be handed to this ping.
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pingCancel()
	result, err := client.Ping(pingCtx, "still here")
	if err != nil {
		t.Fatalf("ping after an abandoned call: %v", err)
	}
	if result.Message != "pong: still here" {
		t.Errorf("Message = %q, want %q", result.Message, "pong: still here")
	}
}

func TestServerGoneFailsPendingAndFutureCalls(t *testing.T) {
	server := newFakeServer(t)
	started := make(chan struct{})
	server.handle("slow", func(json.RawMessage) (any, *Error) {
		close(started)
		select {} // never answers; the connection dies underneath it
	})
	client := dialTest(t, server, nil)

	callDone := make(chan error, 1)
	go func() { callDone <- client.Call(context.Background(), "slow", nil, nil) }()
	<-started
	server.hangUp()

	select {
	case err := <-callDone:
		if !errors.Is(err, ErrClosed) {
			t.Errorf("in-flight call err = %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight call did not fail after the server hung up")
	}

	select {
	case <-client.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done() did not close after the server hung up")
	}
	if err := client.Err(); !errors.Is(err, ErrClosed) {
		t.Errorf("Err() = %v, want ErrClosed", err)
	}
	if err := client.Call(context.Background(), MethodPing, nil, nil); !errors.Is(err, ErrClosed) {
		t.Errorf("post-hangup call err = %v, want ErrClosed", err)
	}
}

func TestUndecodableMessageDoesNotKillTheConnection(t *testing.T) {
	// Framing is length-delimited, so a body that will not decode costs us
	// that one message and nothing else. Tearing the connection down would
	// turn an unrecognised message into the loss of every in-flight call and
	// every subscription.
	server := newFakeServer(t)
	client := dialTest(t, server, nil)
	sub := client.Subscribe()
	t.Cleanup(sub.Close)

	// A JSON-RPC id may legitimately be a string; this client numbers its own
	// ids, so such a message is undecodable rather than merely unknown.
	server.sendRaw(`{"jsonrpc":"2.0","id":"a-string-id","result":{}}`)
	server.sendRaw(`{not json at all`)

	// The connection must still work afterwards.
	result, err := client.Ping(context.Background(), "still here")
	if err != nil {
		t.Fatalf("ping after an undecodable message: %v", err)
	}
	if result.Message != "pong: still here" {
		t.Errorf("Message = %q", result.Message)
	}
	select {
	case <-client.Done():
		t.Fatal("connection closed because of an undecodable message")
	default:
	}
	// The subscription must be untouched: neither fed the garbage nor ended.
	select {
	case notification, ok := <-sub.C():
		if !ok {
			t.Fatalf("subscription ended: %v", sub.Err())
		}
		t.Errorf("an undecodable message was delivered as a notification: %s", notification.Method)
	default:
	}

	// The drops must be visible rather than silent.
	if got := client.MalformedFrames(); got != 2 {
		t.Errorf("MalformedFrames() = %d, want 2", got)
	}
}

func TestNotificationsReachEverySubscriber(t *testing.T) {
	server := newFakeServer(t)
	client := dialTest(t, server, nil)
	first := client.Subscribe()
	second := client.Subscribe()
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)

	server.notify(MethodSessionLifecycle, LifecycleNotification{
		Type:      LifecycleSessionForeground,
		SessionID: "sess-1",
		Metadata:  &LifecycleMetadata{StartTime: time.Unix(0, 0).UTC()},
	})

	for name, sub := range map[string]*Subscription{"first": first, "second": second} {
		select {
		case notification := <-sub.C():
			if notification.Method != MethodSessionLifecycle {
				t.Errorf("%s: Method = %q", name, notification.Method)
			}
			lifecycle, err := notification.Lifecycle()
			if err != nil {
				t.Fatalf("%s: decode lifecycle: %v", name, err)
			}
			if lifecycle.Type != LifecycleSessionForeground || lifecycle.SessionID != "sess-1" {
				t.Errorf("%s: lifecycle = %+v", name, lifecycle)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no notification", name)
		}
	}
}

func TestSessionEventNotificationDecodes(t *testing.T) {
	server := newFakeServer(t)
	client := dialTest(t, server, nil)
	sub := client.Subscribe()
	t.Cleanup(sub.Close)

	server.notify(MethodSessionEvent, json.RawMessage(
		`{"sessionId":"sess-1","event":{"type":"assistant.turn_end","id":"evt-9","timestamp":"2026-08-07T22:08:30.203Z","data":{"tokens":12}}}`))

	select {
	case notification := <-sub.C():
		event, err := notification.SessionEvent()
		if err != nil {
			t.Fatalf("decode session event: %v", err)
		}
		if event.SessionID != "sess-1" || event.Event.Type != "assistant.turn_end" || event.Event.ID != "evt-9" {
			t.Errorf("event = %+v", event)
		}
		// Data stays raw so unknown event shapes survive.
		if !strings.Contains(string(event.Event.Data), `"tokens":12`) {
			t.Errorf("Data = %s", event.Event.Data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notification")
	}
}

func TestSlowSubscriberIsDroppedLoudly(t *testing.T) {
	server := newFakeServer(t)
	client := dialTest(t, server, &Options{SubscriptionBuffer: 2})
	slow := client.Subscribe()

	for range 20 {
		server.notify(MethodSessionEvent, json.RawMessage(`{"sessionId":"s","event":{"type":"tick"}}`))
	}

	// Drain until the channel closes; the subscription must end rather than
	// quietly skip events.
	deadline := time.After(5 * time.Second)
	for open := true; open; {
		select {
		case _, ok := <-slow.C():
			open = ok
		case <-deadline:
			t.Fatal("subscription never closed despite overrunning its buffer")
		}
	}
	if err := slow.Err(); !errors.Is(err, ErrSubscriptionOverrun) {
		t.Errorf("Err() = %v, want ErrSubscriptionOverrun", err)
	}

	// The connection itself must survive one subscriber falling behind.
	if _, err := client.Ping(context.Background(), "alive"); err != nil {
		t.Errorf("client unusable after dropping a slow subscriber: %v", err)
	}
}

func TestSubscriptionEndsWhenServerGoes(t *testing.T) {
	server := newFakeServer(t)
	client := dialTest(t, server, nil)
	sub := client.Subscribe()

	server.hangUp()
	select {
	case _, ok := <-sub.C():
		if ok {
			t.Error("received a notification after hangup")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscription did not close when the server went away")
	}
	if err := sub.Err(); !errors.Is(err, ErrClosed) {
		t.Errorf("Err() = %v, want ErrClosed", err)
	}
}

func TestSubscribeAfterCloseReturnsClosedSubscription(t *testing.T) {
	server := newFakeServer(t)
	client := dialTest(t, server, nil)
	_ = client.Close()

	sub := client.Subscribe()
	select {
	case _, ok := <-sub.C():
		if ok {
			t.Error("a subscription on a closed client delivered a notification")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a subscription on a closed client never closed")
	}
}

func TestServerRequestIsAnswered(t *testing.T) {
	// Copilot issues requests to the client for permission and user-input
	// prompts. We opt out of those, but an unanswered request would leave the
	// server's turn waiting forever, so it must get a reply either way.
	server := newFakeServer(t)
	dialTest(t, server, nil)

	server.request(7001, "userInput.request")
	select {
	case reply := <-server.clientReplies:
		if reply.ID == nil || *reply.ID != 7001 {
			t.Fatalf("reply ID = %v, want 7001", reply.ID)
		}
		if reply.Error == nil {
			t.Fatal("reply carried no error object")
		}
		if reply.Error.Code != CodeMethodNotFound {
			t.Errorf("Code = %d, want %d", reply.Error.Code, CodeMethodNotFound)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client never answered the server's request")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	server := newFakeServer(t)
	client := dialTest(t, server, nil)
	sub := client.Subscribe()

	for range 3 {
		if err := client.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		sub.Close()
	}
	if err := client.Err(); !errors.Is(err, ErrClosed) {
		t.Errorf("Err() = %v, want ErrClosed", err)
	}
}

func TestDialRetryGivesUpOnProtocolMismatch(t *testing.T) {
	// Retrying a version mismatch would just fail identically until the
	// context expired, hiding the real cause behind a timeout.
	server := newFakeServer(t)
	server.handle(MethodConnect, func(json.RawMessage) (any, *Error) {
		return ConnectResult{OK: true, ProtocolVersion: SupportedProtocolVersion + 1, Version: "9.9.9"}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, err := DialRetry(ctx, server.addr(), nil)
	if !errors.Is(err, ErrProtocolVersion) {
		t.Fatalf("err = %v, want ErrProtocolVersion", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("DialRetry retried a version mismatch for %v", elapsed)
	}
}

func TestDialRetryWaitsForServer(t *testing.T) {
	// The embedded server binds its port seconds after the process starts, so
	// the first dials legitimately fail.
	server := newFakeServer(t)
	address := server.addr()
	server.close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := DialRetry(ctx, address, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	// The last dial failure is kept, so the operator learns why, not just that
	// time ran out.
	if !strings.Contains(err.Error(), "last attempt") {
		t.Errorf("error %q does not report the underlying dial failure", err)
	}
}
