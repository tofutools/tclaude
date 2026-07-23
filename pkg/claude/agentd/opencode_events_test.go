package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestOpenCodeEventProjectorMapping(t *testing.T) {
	const convID = "ses_target"
	tests := []struct {
		name       string
		event      string
		wantEvents []string
		wantTool   string
		wantDetail string
	}{
		{
			name:       "session created",
			event:      openCodeTestEvent("evt_created", "session.created", convID, ``),
			wantEvents: []string{"SessionStart"},
		},
		{
			name: "busy",
			event: openCodeTestEvent("evt_busy", "session.status", convID,
				`"status":{"type":"busy"}`),
			wantEvents: []string{"UserPromptSubmit"},
		},
		{
			name: "retry remains working",
			event: openCodeTestEvent("evt_retry", "session.status", convID,
				`"status":{"type":"retry","attempt":2,"message":"again"}`),
			wantEvents: []string{"UserPromptSubmit"},
		},
		{
			name: "idle",
			event: openCodeTestEvent("evt_idle", "session.status", convID,
				`"status":{"type":"idle"}`),
			wantEvents: []string{"Stop"},
		},
		{
			name:       "legacy idle",
			event:      openCodeTestEvent("evt_legacy_idle", "session.idle", convID, ``),
			wantEvents: []string{"Stop"},
		},
		{
			name: "tool running",
			event: openCodeTestEvent("evt_tool_running", "message.part.updated", convID,
				`"part":{"id":"prt_1","type":"tool","tool":"bash","callID":"call_1",`+
					`"state":{"status":"running","input":{"command":"pwd"}}}`),
			wantEvents: []string{"PreToolUse"},
			wantTool:   "Bash",
		},
		{
			name: "tool completed",
			event: openCodeTestEvent("evt_tool_done", "message.part.updated", convID,
				`"part":{"id":"prt_1","type":"tool","tool":"edit","callID":"call_1",`+
					`"state":{"status":"completed","input":{"filePath":"/tmp/a"}}}`),
			wantEvents: []string{"PostToolUse"},
			wantTool:   "Edit",
		},
		{
			name: "tool failed",
			event: openCodeTestEvent("evt_tool_error", "message.part.updated", convID,
				`"part":{"id":"prt_1","type":"tool","tool":"read","callID":"call_1",`+
					`"state":{"status":"error","input":{"filePath":"/tmp/a"},"error":"denied"}}`),
			wantEvents: []string{"PostToolUseFailure"},
			wantTool:   "Read",
		},
		{
			name: "question",
			event: openCodeTestEvent("evt_question", "question.asked", convID,
				`"id":"que_1","questions":[{"question":"Choose one?"}]`),
			wantEvents: []string{"Notification"},
			wantDetail: "Choose one?",
		},
		{
			name: "permission",
			event: openCodeTestEvent("evt_permission", "permission.asked", convID,
				`"id":"per_1","permission":"external_directory"`),
			wantEvents: []string{"PermissionRequest"},
			wantTool:   "external_directory",
		},
		{
			name: "permission replied resumes work",
			event: openCodeTestEvent("evt_permission_reply", "permission.replied", convID,
				`"requestID":"per_1","reply":"once"`),
			wantEvents: []string{"UserPromptSubmit"},
		},
		{
			name: "v2 permission",
			event: openCodeTestEvent("evt_permission_v2", "permission.v2.asked", convID,
				`"id":"per_2","action":"read outside workspace"`),
			wantEvents: []string{"PermissionRequest"},
			wantTool:   "read outside workspace",
		},
		{
			name: "provider auth error",
			event: openCodeTestEvent("evt_error", "session.error", convID,
				`"error":{"name":"ProviderAuthError","data":{"message":"login required"}}`),
			wantEvents: []string{"StopFailure"},
			wantDetail: "authentication_failed",
		},
		{
			name: "question replied resumes work",
			event: openCodeTestEvent("evt_question_reply", "question.replied", convID,
				`"requestID":"que_1","answers":[["yes"]]`),
			wantEvents: []string{"UserPromptSubmit"},
		},
		{
			name:  "conversation deletion is not process exit",
			event: openCodeTestEvent("evt_deleted", "session.deleted", convID, ``),
		},
		{
			name: "foreign session dropped",
			event: openCodeTestEvent("evt_foreign", "session.status", "ses_other",
				`"status":{"type":"busy"}`),
		},
		{
			name: "blank session dropped",
			event: openCodeTestEvent("evt_blank", "session.status", "",
				`"status":{"type":"busy"}`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projector := newOpenCodeEventProjector(convID, "/tmp/project")
			got, err := projector.project(json.RawMessage(tt.event))
			require.NoError(t, err)
			var names []string
			for _, input := range got {
				names = append(names, input.HookEventName)
				assert.Equal(t, convID, input.ConvID)
				assert.Equal(t, "/tmp/project", input.Cwd)
			}
			assert.Equal(t, tt.wantEvents, names)
			if len(got) == 0 {
				return
			}
			switch got[0].HookEventName {
			case "Notification":
				assert.Equal(t, "elicitation_dialog", got[0].NotificationType)
				assert.Equal(t, tt.wantDetail, got[0].Message)
			case "StopFailure":
				assert.Equal(t, tt.wantDetail, got[0].ErrorType)
			default:
				assert.Equal(t, tt.wantTool, got[0].ToolName)
			}
			if tt.name == "tool completed" {
				assert.JSONEq(t, `{"filePath":"/tmp/a","file_path":"/tmp/a"}`,
					string(got[0].ToolInput))
			}
		})
	}
}

func TestOpenCodeEventProjectorDeduplicatesAndDefersIdleUntilToolsFinish(t *testing.T) {
	const convID = "ses_target"
	projector := newOpenCodeEventProjector(convID, "/tmp/project")
	running := openCodeTestEvent("evt_running", "message.part.updated", convID,
		`"part":{"id":"prt_1","type":"tool","tool":"bash","callID":"call_1",`+
			`"state":{"status":"running","input":{"command":"sleep 1"}}}`)

	got, err := projector.project(json.RawMessage(running))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "PreToolUse", got[0].HookEventName)

	got, err = projector.project(json.RawMessage(running))
	require.NoError(t, err)
	assert.Empty(t, got, "the same SSE event id is replay-safe")

	got, err = projector.project(json.RawMessage(openCodeTestEvent(
		"evt_running_snapshot", "message.part.updated", convID,
		`"part":{"id":"prt_1","type":"tool","tool":"bash","callID":"call_1",`+
			`"state":{"status":"running","input":{"command":"sleep 1"},"metadata":{"output":"tick"}}}`)))
	require.NoError(t, err)
	assert.Empty(t, got, "repeated running snapshots do not churn hook state")

	got, err = projector.project(json.RawMessage(openCodeTestEvent(
		"evt_early_idle", "session.status", convID, `"status":{"type":"idle"}`)))
	require.NoError(t, err)
	assert.Empty(t, got, "idle is held while a tool call remains open")

	got, err = projector.project(json.RawMessage(openCodeTestEvent(
		"evt_completed", "message.part.updated", convID,
		`"part":{"id":"prt_1","type":"tool","tool":"bash","callID":"call_1",`+
			`"state":{"status":"completed","input":{"command":"sleep 1"}}}`)))
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "PostToolUse", got[0].HookEventName)
	assert.Equal(t, "Stop", got[1].HookEventName,
		"the last terminal tool event drains the deferred idle boundary")

	got, err = projector.project(json.RawMessage(openCodeTestEvent(
		"evt_legacy_idle", "session.idle", convID, ``)))
	require.NoError(t, err)
	assert.Empty(t, got, "session.status(idle) plus session.idle produces one Stop")
}

func TestOpenCodeEventProjectorSessionFilteringIsStateIsolated(t *testing.T) {
	const convID = "ses_target"
	projector := newOpenCodeEventProjector(convID, "/tmp/project")

	for _, event := range []string{
		openCodeTestEvent("evt_foreign_busy", "session.status", "ses_other",
			`"status":{"type":"busy"}`),
		openCodeTestEvent("evt_foreign_tool", "message.part.updated", "ses_other",
			`"part":{"id":"prt_other","type":"tool","tool":"bash","callID":"call_other",`+
				`"state":{"status":"running","input":{"command":"sleep 1"}}}`),
		openCodeTestEvent("evt_missing_session", "session.status", "",
			`"status":{"type":"idle"}`),
	} {
		got, err := projector.project(json.RawMessage(event))
		require.NoError(t, err)
		assert.Empty(t, got)
	}

	got, err := projector.project(json.RawMessage(openCodeTestEvent(
		"evt_target_idle", "session.status", convID, `"status":{"type":"idle"}`)))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "Stop", got[0].HookEventName,
		"a foreign session's running tool must not defer the target session's idle")
}

func TestOpenCodeProjectionDrivesSharedStatusStateMachine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()

	const (
		sessionID = "spwn-opencode"
		convID    = "ses_target"
		cwd       = "/tmp/project"
	)
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: sessionID, TmuxSession: sessionID, ConvID: convID, Cwd: cwd,
		Harness: harness.OpenCodeName, Status: session.StatusIdle,
	}))
	runtime := db.OpenCodeRuntime{SessionID: sessionID, ConvID: convID, Cwd: cwd}
	projector := newOpenCodeEventProjector(convID, cwd)
	apply := func(event string) *session.SessionState {
		t.Helper()
		consumeOpenCodeEvent(context.Background(), runtime, projector, json.RawMessage(event))
		state, err := session.LoadSessionState(sessionID)
		require.NoError(t, err)
		return state
	}

	state := apply(openCodeTestEvent("evt_busy", "session.status", convID,
		`"status":{"type":"busy"}`))
	assert.Equal(t, session.StatusWorking, state.Status)

	state = apply(openCodeTestEvent("evt_tool", "message.part.updated", convID,
		`"part":{"id":"prt_1","type":"tool","tool":"bash","callID":"call_1",`+
			`"state":{"status":"running","input":{"command":"sleep 1"}}}`))
	assert.Equal(t, session.StatusWorking, state.Status)
	assert.Equal(t, "Bash", state.StatusDetail)

	state = apply(openCodeTestEvent("evt_question", "question.asked", convID,
		`"id":"que_1","questions":[{"question":"Continue?"}]`))
	assert.Equal(t, session.StatusAwaitingInput, state.Status)
	assert.Equal(t, "Continue?", state.StatusDetail)

	state = apply(openCodeTestEvent("evt_question_reply", "question.replied", convID,
		`"requestID":"que_1","answers":[["yes"]]`))
	assert.Equal(t, session.StatusWorking, state.Status)

	state = apply(openCodeTestEvent("evt_permission", "permission.asked", convID,
		`"id":"per_1","permission":"external_directory"`))
	assert.Equal(t, session.StatusAwaitingPermission, state.Status)

	state = apply(openCodeTestEvent("evt_permission_reply", "permission.replied", convID,
		`"requestID":"per_1","reply":"once"`))
	assert.Equal(t, session.StatusWorking, state.Status)

	state = apply(openCodeTestEvent("evt_early_idle", "session.status", convID,
		`"status":{"type":"idle"}`))
	assert.Equal(t, session.StatusWorking, state.Status,
		"an early idle cannot hide a still-running tool")

	state = apply(openCodeTestEvent("evt_done", "message.part.updated", convID,
		`"part":{"id":"prt_1","type":"tool","tool":"bash","callID":"call_1",`+
			`"state":{"status":"completed","input":{"command":"sleep 1"}}}`))
	assert.Equal(t, session.StatusIdle, state.Status)
	assert.Empty(t, state.StatusDetail)

	state = apply(openCodeTestEvent("evt_error", "session.error", convID,
		`"error":{"name":"APIError","data":{"message":"limited","statusCode":429}}`))
	assert.Equal(t, session.StatusError, state.Status)
	assert.Equal(t, "rate_limit", state.StatusDetail)

	assert.Equal(t, harness.OpenCodeName, state.Harness)
}

func TestConsumeOpenCodeEventWaitsForLaunchSessionRow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()

	const (
		sessionID = "spwn-opencode-row-race"
		convID    = "ses_row_race"
		cwd       = "/tmp/project"
	)
	runtime := db.OpenCodeRuntime{SessionID: sessionID, ConvID: convID, Cwd: cwd}
	projector := newOpenCodeEventProjector(convID, cwd)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		consumeOpenCodeEvent(ctx, runtime, projector, json.RawMessage(openCodeTestEvent(
			"evt_busy", "session.status", convID, `"status":{"type":"busy"}`)))
	}()

	time.Sleep(50 * time.Millisecond)
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: sessionID, TmuxSession: sessionID, ConvID: convID, Cwd: cwd,
		Harness: harness.OpenCodeName, Status: session.StatusIdle,
	}))
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("projected hook did not resume after the launch row appeared")
	}
	state, err := session.LoadSessionState(sessionID)
	require.NoError(t, err)
	assert.Equal(t, session.StatusWorking, state.Status)
}

func TestConsumeOpenCodeSSEReconnectsReconcilesAttentionAndStopsWithContext(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()

	const (
		sessionID = "spwn-opencode-reconnect"
		convID    = "ses_reconnect"
		password  = "private-password"
	)
	var eventConnections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		require.True(t, ok)
		assert.Equal(t, openCodeServerUsername, user)
		assert.Equal(t, password, pass)
		switch r.URL.Path {
		case "/event":
			connection := eventConnections.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if connection == 1 {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", openCodeTestEvent(
					"evt_busy", "session.status", convID, `"status":{"type":"busy"}`))
				return
			}
			_, _ = w.Write([]byte("data: {\"id\":\"evt_connected\",\"type\":\"server.connected\",\"properties\":{}}\n\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/session/status":
			_, _ = fmt.Fprintf(w, `{"%s":{"type":"busy"}}`, convID)
		case "/question":
			if eventConnections.Load() >= 2 {
				_, _ = fmt.Fprintf(w,
					`[{"id":"que_reconnect","sessionID":"%s","questions":[{"question":"Choose?"}]}]`,
					convID)
			} else {
				_, _ = w.Write([]byte("[]"))
			}
		case "/permission":
			_, _ = w.Write([]byte("[]"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	runtime := db.OpenCodeRuntime{
		SessionID: sessionID, ConvID: convID, Cwd: "/tmp/project",
		PID: os.Getpid(), ServerURL: server.URL, Password: password,
	}
	require.True(t, openCodeProcessOwnsEndpoint(runtime.PID, runtime.ServerURL))
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID: sessionID, TmuxSession: sessionID, ConvID: convID, Cwd: runtime.Cwd,
		Harness: harness.OpenCodeName, Status: session.StatusIdle,
	}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		consumeOpenCodeSSEWithRetry(ctx, runtime, 5*time.Millisecond)
	}()

	require.Eventually(t, func() bool {
		state, err := session.LoadSessionState(sessionID)
		return err == nil && state.Status == session.StatusAwaitingInput &&
			state.StatusDetail == "Choose?"
	}, 2*time.Second, 10*time.Millisecond)
	assert.GreaterOrEqual(t, eventConnections.Load(), int32(2))

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SSE consumer did not stop after context cancellation")
	}
	connectionsAtCancel := eventConnections.Load()
	time.Sleep(25 * time.Millisecond)
	assert.Equal(t, connectionsAtCancel, eventConnections.Load(),
		"cancelled consumer must not reconnect")
}

func TestReconcileOpenCodeSSEClearsAnsweredAttentionWhileDisconnected(t *testing.T) {
	for _, tt := range []struct {
		status string
		want   string
	}{
		{status: "busy", want: session.StatusWorking},
		{status: "idle", want: session.StatusIdle},
	} {
		t.Run(tt.status, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			db.ResetForTest()

			const (
				sessionID = "spwn-opencode-answered"
				convID    = "ses_answered"
				password  = "private-password"
			)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/session/status":
					_, _ = fmt.Fprintf(w, `{"%s":{"type":%q}}`, convID, tt.status)
				case "/question", "/permission":
					_, _ = w.Write([]byte("[]"))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			runtime := db.OpenCodeRuntime{
				SessionID: sessionID, ConvID: convID, Cwd: "/tmp/project",
				PID: os.Getpid(), ServerURL: server.URL, Password: password,
			}
			require.NoError(t, session.SaveSessionState(&session.SessionState{
				ID: sessionID, TmuxSession: sessionID, ConvID: convID, Cwd: runtime.Cwd,
				Harness: harness.OpenCodeName, Status: session.StatusAwaitingPermission,
				StatusDetail: "external_directory",
			}))
			projector := newOpenCodeEventProjector(convID, runtime.Cwd)
			projector.lastSessionStatus = tt.status

			require.NoError(t, reconcileOpenCodeSSE(context.Background(), runtime, projector))
			state, err := session.LoadSessionState(sessionID)
			require.NoError(t, err)
			assert.Equal(t, tt.want, state.Status,
				"authoritative status must clear attention answered while SSE was down")
			if tt.want == session.StatusIdle {
				assert.Empty(t, state.StatusDetail)
			}
		})
	}
}

func openCodeTestEvent(id, eventType, sessionID, additionalProperties string) string {
	properties := fmt.Sprintf(`"sessionID":%q`, sessionID)
	if additionalProperties != "" {
		properties += "," + additionalProperties
	}
	return fmt.Sprintf(`{"id":%q,"type":%q,"properties":{%s}}`,
		id, eventType, properties)
}
