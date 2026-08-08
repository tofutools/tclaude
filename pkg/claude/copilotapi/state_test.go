package copilotapi

import (
	"context"
	"encoding/json"
	"testing"
)

// The three point-in-time state reads. Each is asserted against the payload
// shape a live 1.0.78 server was observed returning, because the whole reason
// these exist is that a consumer reconstructing the same answers from the event
// stream gets them wrong in ways that look right.

func TestIsProcessingReportsTheServersAnswer(t *testing.T) {
	server := newFakeServer(t)
	var captured map[string]string
	answer := true
	server.handle(MethodSessionIsProcessing, func(params json.RawMessage) (any, *Error) {
		if err := json.Unmarshal(params, &captured); err != nil {
			return nil, &Error{Code: CodeInvalidParams, Message: err.Error()}
		}
		return map[string]bool{"processing": answer}, nil
	})
	client := dialTest(t, server, nil)

	processing, err := client.IsProcessing(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("IsProcessing: %v", err)
	}
	if !processing {
		t.Error("processing = false, want true")
	}
	if captured["sessionId"] != "sess-1" {
		t.Errorf("sessionId = %q", captured["sessionId"])
	}

	answer = false
	processing, err = client.IsProcessing(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("IsProcessing: %v", err)
	}
	if processing {
		t.Error("processing = true after the server said false")
	}
}

func TestActivityDecodesBothFlags(t *testing.T) {
	server := newFakeServer(t)
	server.handle(MethodSessionActivity, func(json.RawMessage) (any, *Error) {
		return map[string]bool{"abortable": true, "hasActiveWork": true}, nil
	})
	client := dialTest(t, server, nil)

	activity, err := client.Activity(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if !activity.Abortable || !activity.HasActiveWork {
		t.Errorf("activity = %+v, want both flags set", activity)
	}
}

// The live payload, verbatim. The nesting under "request" is the part a
// hand-written struct gets wrong, and getting it wrong here would report an
// agent as unblocked while a human is being waited on.
func TestPendingPermissionRequestsDecodesTheLiveShape(t *testing.T) {
	const live = `{"items":[{
		"requestId":"aef407eb-1eee-4fdd-a808-b85833d1eb66",
		"request":{"kind":"commands","fullCommandText":"sleep 8; echo done",
			"intention":"Sleep for 8 seconds then print 'done'",
			"commandIdentifiers":["sleep"],"canOfferSessionApproval":true,
			"toolCallId":"call_Ts9tKutFtmZm4Y2pvEyDmgqZ"}}]}`

	server := newFakeServer(t)
	server.handle(MethodSessionPermissions, func(json.RawMessage) (any, *Error) {
		return json.RawMessage(live), nil
	})
	client := dialTest(t, server, nil)

	items, err := client.PendingPermissionRequests(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("PendingPermissionRequests: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if items[0].RequestID != "aef407eb-1eee-4fdd-a808-b85833d1eb66" {
		t.Errorf("RequestID = %q", items[0].RequestID)
	}
	// Raw rather than modelled: the union has nine arms sharing almost nothing,
	// and a caller that only needs "is a human being waited on" needs none of
	// them. What must survive is the payload, so a caller that DOES want a
	// detail can still get one.
	var detail struct {
		Kind      string `json:"kind"`
		Intention string `json:"intention"`
	}
	if err := json.Unmarshal(items[0].Request, &detail); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if detail.Kind != "commands" || detail.Intention == "" {
		t.Errorf("request detail = %+v", detail)
	}
}

// An empty list is the ordinary state, and it must not be reported as an error
// or as a nil that a caller might read as "unknown". Under --allow-all-tools it
// is what every read returns.
func TestPendingPermissionRequestsIsEmptyWhenNothingIsWaiting(t *testing.T) {
	server := newFakeServer(t)
	server.handle(MethodSessionPermissions, func(json.RawMessage) (any, *Error) {
		return json.RawMessage(`{"items":[]}`), nil
	})
	client := dialTest(t, server, nil)

	items, err := client.PendingPermissionRequests(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("PendingPermissionRequests: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("items = %d, want 0", len(items))
	}
}
