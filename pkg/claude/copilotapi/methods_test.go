package copilotapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestPingEchoesMessage(t *testing.T) {
	server := newFakeServer(t)
	server.handle(MethodPing, func(params json.RawMessage) (any, *Error) {
		var request PingParams
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, &Error{Code: CodeInvalidParams, Message: err.Error()}
		}
		return PingResult{
			Message:         "pong: " + request.Message,
			Timestamp:       "2026-08-07T22:08:30.077Z",
			ProtocolVersion: SupportedProtocolVersion,
		}, nil
	})
	client := dialTest(t, server, nil)

	result, err := client.Ping(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if result.Message != "pong: hello" {
		t.Errorf("Message = %q, want %q", result.Message, "pong: hello")
	}
	if result.ProtocolVersion != SupportedProtocolVersion {
		t.Errorf("ProtocolVersion = %d", result.ProtocolVersion)
	}
}

func TestCreateSessionSendsTheFieldsTheServerNeeds(t *testing.T) {
	server := newFakeServer(t)
	var captured CreateSessionParams
	server.handle(MethodSessionCreate, func(params json.RawMessage) (any, *Error) {
		if err := json.Unmarshal(params, &captured); err != nil {
			return nil, &Error{Code: CodeInvalidParams, Message: err.Error()}
		}
		return SessionInfo{
			SessionID:     captured.SessionID,
			WorkspacePath: "/state/" + captured.SessionID,
			Capabilities:  json.RawMessage(`{"ui":{"elicitation":true}}`),
		}, nil
	})
	client := dialTest(t, server, nil)

	info, err := client.CreateSession(context.Background(), CreateSessionParams{
		WorkingDirectory: "/work",
		ClientName:       "tclaude",
		Streaming:        true,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// An empty SessionID must be filled in, since the server echoes back
	// whatever we chose rather than assigning one.
	if captured.SessionID == "" {
		t.Fatal("CreateSession sent an empty sessionId")
	}
	if info.SessionID != captured.SessionID {
		t.Errorf("SessionID = %q, want %q", info.SessionID, captured.SessionID)
	}
	if captured.WorkingDirectory != "/work" || captured.ClientName != "tclaude" || !captured.Streaming {
		t.Errorf("params = %+v", captured)
	}
	if info.WorkspacePath != "/state/"+captured.SessionID {
		t.Errorf("WorkspacePath = %q", info.WorkspacePath)
	}
}

func TestCreateSessionRejectsMismatchedID(t *testing.T) {
	// Continuing here would leave us driving a session other than the one we
	// recorded, which is worse than failing.
	server := newFakeServer(t)
	server.handle(MethodSessionCreate, func(json.RawMessage) (any, *Error) {
		return SessionInfo{SessionID: "somebody-elses-session"}, nil
	})
	client := dialTest(t, server, nil)

	_, err := client.CreateSession(context.Background(), CreateSessionParams{SessionID: "ours"})
	if err == nil {
		t.Fatal("CreateSession accepted a session ID it did not ask for")
	}
	if !strings.Contains(err.Error(), "somebody-elses-session") || !strings.Contains(err.Error(), "ours") {
		t.Errorf("error %q does not name both session IDs", err)
	}
}

func TestCreateSessionRejectsMissingID(t *testing.T) {
	server := newFakeServer(t)
	server.handle(MethodSessionCreate, func(json.RawMessage) (any, *Error) {
		return map[string]string{"workspacePath": "/state/x"}, nil
	})
	client := dialTest(t, server, nil)

	if _, err := client.CreateSession(context.Background(), CreateSessionParams{SessionID: "ours"}); err == nil {
		t.Fatal("CreateSession accepted a reply with no sessionId")
	}
}

func TestSetForegroundSessionSurfacesInBandFailure(t *testing.T) {
	// The server reports refusal as a successful response carrying
	// success:false, so a client that only checked for a JSON-RPC error would
	// believe it had switched the TUI when it had not.
	server := newFakeServer(t)
	server.handle(MethodSessionSetFg, func(json.RawMessage) (any, *Error) {
		return SetForegroundResult{Success: false, Error: "session is not attachable"}, nil
	})
	client := dialTest(t, server, nil)

	err := client.SetForegroundSession(context.Background(), "sess-1")
	if err == nil {
		t.Fatal("SetForegroundSession reported success for success:false")
	}
	if !strings.Contains(err.Error(), "session is not attachable") {
		t.Errorf("error %q drops the server's reason", err)
	}
}

func TestSetForegroundSessionFailsWithoutAReason(t *testing.T) {
	server := newFakeServer(t)
	server.handle(MethodSessionSetFg, func(json.RawMessage) (any, *Error) {
		return SetForegroundResult{Success: false}, nil
	})
	client := dialTest(t, server, nil)

	if err := client.SetForegroundSession(context.Background(), "sess-1"); err == nil {
		t.Fatal("SetForegroundSession reported success for a bare success:false")
	}
}

func TestSetForegroundSessionSucceeds(t *testing.T) {
	server := newFakeServer(t)
	var captured map[string]string
	server.handle(MethodSessionSetFg, func(params json.RawMessage) (any, *Error) {
		_ = json.Unmarshal(params, &captured)
		return SetForegroundResult{Success: true}, nil
	})
	client := dialTest(t, server, nil)

	if err := client.SetForegroundSession(context.Background(), "sess-1"); err != nil {
		t.Fatalf("SetForegroundSession: %v", err)
	}
	if captured["sessionId"] != "sess-1" {
		t.Errorf("params = %v", captured)
	}
}

func TestGetForegroundSessionReportsTheTUISession(t *testing.T) {
	server := newFakeServer(t)
	server.handle(MethodSessionGetFg, func(json.RawMessage) (any, *Error) {
		return SessionInfo{SessionID: "tui-session", WorkspacePath: "/state/tui-session"}, nil
	})
	client := dialTest(t, server, nil)

	info, err := client.GetForegroundSession(context.Background())
	if err != nil {
		t.Fatalf("GetForegroundSession: %v", err)
	}
	if info.SessionID != "tui-session" {
		t.Errorf("SessionID = %q", info.SessionID)
	}
}

func TestSendReturnsMessageID(t *testing.T) {
	server := newFakeServer(t)
	var captured SendParams
	server.handle(MethodSessionSend, func(params json.RawMessage) (any, *Error) {
		if err := json.Unmarshal(params, &captured); err != nil {
			return nil, &Error{Code: CodeInvalidParams, Message: err.Error()}
		}
		return SendResult{MessageID: "msg-1"}, nil
	})
	client := dialTest(t, server, nil)

	messageID, err := client.Send(context.Background(), SendParams{
		SessionID:     "sess-1",
		Prompt:        "full prompt",
		DisplayPrompt: "shown in the TUI",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if messageID != "msg-1" {
		t.Errorf("messageID = %q", messageID)
	}
	if captured.Prompt != "full prompt" || captured.DisplayPrompt != "shown in the TUI" {
		t.Errorf("params = %+v", captured)
	}
}

func TestSetSessionNameTakesANullResult(t *testing.T) {
	// session.name.set answers with a literal null, which must not be
	// mistaken for a decode failure.
	server := newFakeServer(t)
	var captured SetNameParams
	server.handle(MethodSessionNameSet, func(params json.RawMessage) (any, *Error) {
		_ = json.Unmarshal(params, &captured)
		return nil, nil
	})
	client := dialTest(t, server, nil)

	if err := client.SetSessionName(context.Background(), "sess-1", "a title"); err != nil {
		t.Fatalf("SetSessionName: %v", err)
	}
	if captured.SessionID != "sess-1" || captured.Name != "a title" {
		t.Errorf("params = %+v", captured)
	}
}

func TestContextInfoHandlesUninitialisedSession(t *testing.T) {
	// Between session.create and the first turn the server answers
	// {"contextInfo": null}. That is a normal state, not an error.
	server := newFakeServer(t)
	server.handle(MethodSessionContextInfo, func(json.RawMessage) (any, *Error) {
		return map[string]any{"contextInfo": nil}, nil
	})
	client := dialTest(t, server, nil)

	info, err := client.ContextInfo(context.Background(), ContextInfoParams{SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("ContextInfo: %v", err)
	}
	if info != nil {
		t.Errorf("ContextInfo = %+v, want nil", info)
	}
}

func TestContextInfoDecodesTokenBreakdown(t *testing.T) {
	server := newFakeServer(t)
	var captured ContextInfoParams
	server.handle(MethodSessionContextInfo, func(params json.RawMessage) (any, *Error) {
		if err := json.Unmarshal(params, &captured); err != nil {
			return nil, &Error{Code: CodeInvalidParams, Message: err.Error()}
		}
		return contextInfoResult{ContextInfo: &ContextInfo{
			ModelName:           "claude-sonnet-4",
			SystemTokens:        1200,
			ConversationTokens:  8000,
			TotalTokens:         9200,
			PromptTokenLimit:    128000,
			CompactionThreshold: 100000,
			Limit:               144000,
		}}, nil
	})
	client := dialTest(t, server, nil)

	info, err := client.ContextInfo(context.Background(), ContextInfoParams{
		SessionID:        "sess-1",
		PromptTokenLimit: 0,
		OutputTokenLimit: 0,
	})
	if err != nil {
		t.Fatalf("ContextInfo: %v", err)
	}
	if info == nil {
		t.Fatal("ContextInfo = nil")
	}
	if info.TotalTokens != 9200 || info.Limit != 144000 || info.CompactionThreshold != 100000 {
		t.Errorf("ContextInfo = %+v", info)
	}
	// Both limits are required by the server even when zero, so they must be
	// on the wire rather than omitted.
	if !strings.Contains(capturedRaw(t, captured), "promptTokenLimit") {
		t.Error("promptTokenLimit was omitted from the request")
	}
}

func capturedRaw(t *testing.T, params ContextInfoParams) string {
	t.Helper()
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(encoded)
}

func TestUsageMetricsDecodes(t *testing.T) {
	server := newFakeServer(t)
	server.handle(MethodSessionUsage, func(json.RawMessage) (any, *Error) {
		return json.RawMessage(`{
			"totalPremiumRequestCost": 1.5,
			"totalUserRequests": 3,
			"totalNanoAiu": 42.0,
			"totalApiDurationMs": 9100,
			"sessionStartTime": "2026-08-07T22:08:30.203Z",
			"codeChanges": {"linesAdded": 10, "linesRemoved": 2, "filesModifiedCount": 1, "filesModified": ["a.go"]},
			"tokenDetails": {"input": {"tokenCount": 100}},
			"modelMetrics": {"gpt-5": {
				"requests": {"count": 3, "cost": 1.5},
				"usage": {"inputTokens": 100, "outputTokens": 20, "cacheReadTokens": 5, "cacheWriteTokens": 7, "reasoningTokens": 9},
				"totalNanoAiu": 42.0
			}},
			"currentModel": "gpt-5",
			"lastCallInputTokens": 100,
			"lastCallOutputTokens": 20
		}`), nil
	})
	client := dialTest(t, server, nil)

	metrics, err := client.UsageMetrics(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("UsageMetrics: %v", err)
	}
	if metrics.TotalPremiumRequestCost != 1.5 || metrics.TotalUserRequests != 3 {
		t.Errorf("metrics = %+v", metrics)
	}
	if metrics.CodeChanges.LinesAdded != 10 || len(metrics.CodeChanges.FilesModified) != 1 {
		t.Errorf("CodeChanges = %+v", metrics.CodeChanges)
	}
	model, ok := metrics.ModelMetrics["gpt-5"]
	if !ok {
		t.Fatalf("ModelMetrics = %+v, want a gpt-5 entry", metrics.ModelMetrics)
	}
	// Token counts live under usage and request counts under requests. A
	// flattened struct would decode this payload without error and report
	// zeros, so the nesting is asserted explicitly.
	if model.Usage.InputTokens != 100 || model.Usage.OutputTokens != 20 {
		t.Errorf("ModelUsage = %+v", model.Usage)
	}
	if model.Usage.CacheReadTokens != 5 || model.Usage.CacheWriteTokens != 7 || model.Usage.ReasoningTokens != 9 {
		t.Errorf("ModelUsage cache/reasoning = %+v", model.Usage)
	}
	if model.Requests.Count != 3 || model.Requests.Cost != 1.5 {
		t.Errorf("ModelRequests = %+v", model.Requests)
	}
	if model.TotalNanoAIU != 42.0 {
		t.Errorf("TotalNanoAIU = %v", model.TotalNanoAIU)
	}
	if detail, ok := metrics.TokenDetails["input"]; !ok || detail.TokenCount != 100 {
		t.Errorf("TokenDetails = %+v", metrics.TokenDetails)
	}
	if metrics.CurrentModel != "gpt-5" {
		t.Errorf("CurrentModel = %q", metrics.CurrentModel)
	}
}

func TestNewSessionIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := NewSessionID()
		if id == "" {
			t.Fatal("NewSessionID returned an empty string")
		}
		if seen[id] {
			t.Fatalf("NewSessionID repeated %q", id)
		}
		seen[id] = true
	}
}
