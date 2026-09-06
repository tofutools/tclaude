package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestOpenCodeCompactUsesManagedTUICommandAPIWithoutKeys(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const (
		conv = "ses_opencode_compact_api"
		tmux = "tmux-opencode-compact-api"
	)
	commands, server := openCodeTUICommandServer(t, f, tmux, false)
	defer server.Close()
	haveOpenCodeControlSession(t, f, conv, "spwn-oc-compact-api", tmux, server.URL)
	f.HaveMember("crew", conv)

	res := f.AsHuman().Compact(conv)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Raw)
	assert.Equal(t, "session.compact", receiveCommand(t, commands))
	assert.Empty(t, f.World.Tmux.Sent(), "OpenCode compact must not use tmux send-keys")
}

func TestOpenCodeStopEndsAuthoritativeServerWithoutAttachmentControl(t *testing.T) {
	f := newFlow(t)
	const (
		conv = "ses_opencode_exit_api"
		tmux = "tmux-opencode-exit-api"
	)
	commands, server := openCodeTUICommandServer(t, f, tmux, true)
	defer server.Close()
	haveOpenCodeControlSession(t, f, conv, "spwn-oc-exit-api", tmux, server.URL)
	t.Cleanup(agentd.SetVerifyOpenCodeRuntimeForStopTest(func(db.OpenCodeRuntime) bool { return true }))
	t.Cleanup(agentd.SetStopExactOpenCodeRuntimeForTest(func(runtime db.OpenCodeRuntime, _ bool) (bool, error) {
		return true, db.DeleteOpenCodeRuntime(runtime.SessionID)
	}))

	action := agentd.StopOneConvWithIntentForTest(conv, db.AgentExitActionStop)
	require.Equal(t, "soft_stopped", action)
	select {
	case command := <-commands:
		t.Fatalf("Stop controlled the attachment instead of the server: %q", command)
	case <-time.After(20 * time.Millisecond):
	}
	assert.False(t, f.World.Tmux.IsAlive(tmux), "Stop releases the selected server's owned attachment after server teardown")
	agentd.WaitForBackgroundForTest()
	stored, err := db.GetOpenCodeRuntime("spwn-oc-exit-api")
	require.NoError(t, err)
	assert.Nil(t, stored, "the selected authoritative server must be stopped")
	assert.Empty(t, f.World.Tmux.Sent(), "OpenCode server Stop must not use tmux send-keys")
	row, err := db.LoadSession("spwn-oc-exit-api")
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, session.StatusExited, row.Status)
}

func TestOpenCodeStopWithoutLiveAttachmentStillEndsExactServer(t *testing.T) {
	f := newFlow(t)
	const (
		conv  = "ses_opencode_server_only_stop"
		tmux  = "tmux-opencode-server-only-stop"
		label = "spwn-oc-server-only-stop"
	)
	commands, server := openCodeTUICommandServer(t, f, tmux, false)
	defer server.Close()
	haveOpenCodeControlSession(t, f, conv, label, tmux, server.URL)
	t.Cleanup(agentd.SetVerifyOpenCodeRuntimeForStopTest(func(db.OpenCodeRuntime) bool { return true }))
	f.MarkOffline(tmux)
	called := false
	t.Cleanup(agentd.SetStopExactOpenCodeRuntimeForTest(func(runtime db.OpenCodeRuntime, _ bool) (bool, error) {
		called = true
		return true, db.DeleteOpenCodeRuntime(runtime.SessionID)
	}))

	action := agentd.StopOneConvWithIntentForTest(conv, db.AgentExitActionStop)
	require.Equal(t, "soft_stopped", action)
	assert.True(t, called, "server Stop must not depend on a live attachment")
	select {
	case command := <-commands:
		t.Fatalf("server-only Stop dispatched attachment control: %q", command)
	default:
	}
}

func TestOpenCodeStopDeadServerRemovesRestartAuthority(t *testing.T) {
	f := newFlow(t)
	const (
		conv  = "ses_opencode_dead_server_stop"
		tmux  = "tmux-opencode-dead-server-stop"
		label = "spwn-oc-dead-server-stop"
	)
	_, server := openCodeTUICommandServer(t, f, tmux, false)
	defer server.Close()
	haveOpenCodeControlSession(t, f, conv, label, tmux, server.URL)
	runtimeRow, err := db.GetOpenCodeRuntime(label)
	require.NoError(t, err)
	require.NotNil(t, runtimeRow)
	runtimeRow.PID = 1 << 30 // deterministically absent on supported hosts
	require.NoError(t, db.UpsertOpenCodeRuntime(*runtimeRow))
	t.Cleanup(agentd.SetVerifyOpenCodeRuntimeForStopTest(func(db.OpenCodeRuntime) bool {
		t.Fatal("a dead exact server must not require live endpoint proof to retire restart authority")
		return false
	}))
	called := false
	t.Cleanup(agentd.SetStopExactOpenCodeRuntimeForTest(func(runtime db.OpenCodeRuntime, _ bool) (bool, error) {
		called = true
		return true, db.DeleteOpenCodeRuntime(runtime.SessionID)
	}))

	action := agentd.StopOneConvWithIntentForTest(conv, db.AgentExitActionStop)
	require.Equal(t, "soft_stopped", action)
	assert.True(t, called, "the exact dead attempt must be torn down, not reported as no execution")
	stored, err := db.GetOpenCodeRuntime(label)
	require.NoError(t, err)
	assert.Nil(t, stored, "Stop must remove the durable authority that lets the reaper restart the server")
	assert.False(t, f.World.Tmux.IsAlive(tmux), "the dead server's owned attachment is cleaned with its restart authority")
	_ = agentd.RunReaperTickForTest(time.Now())
	stored, err = db.GetOpenCodeRuntime(label)
	require.NoError(t, err)
	assert.Nil(t, stored, "a later production reaper sweep must not resurrect the stopped server")
}

func TestOpenCodeStopThenImmediateResumeUsesAuthoritativeCompletion(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := f.AsHuman().SpawnHarness("crew", "OpenCode stop resume", harness.OpenCodeName)
	_, server := openCodeTUICommandServer(t, f, spawn.TmuxSession, false)
	defer server.Close()
	row, err := db.FindSessionByConvID(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, row)
	boundary, err := json.Marshal(session.ExecutionBoundary{
		Version: session.ExecutionBoundaryVersion, LaunchGeneration: row.ExecutionID.String(),
		Harness: session.ExecutionHarness{Name: harness.OpenCodeName},
	})
	require.NoError(t, err)
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: row.ID, ConvID: row.ConvID, ServerURL: server.URL,
		Password: "test-password", PID: os.Getpid(), Cwd: row.Cwd,
		ExecutionBoundaryJSON: string(boundary),
	}))
	t.Cleanup(agentd.SetVerifyOpenCodeRuntimeForStopTest(func(db.OpenCodeRuntime) bool { return true }))
	t.Cleanup(agentd.SetStopExactOpenCodeRuntimeForTest(func(runtime db.OpenCodeRuntime, _ bool) (bool, error) {
		return true, db.DeleteOpenCodeRuntime(runtime.SessionID)
	}))

	require.Equal(t, "soft_stopped", agentd.StopOneConvWithIntentForTest(spawn.ConvID, db.AgentExitActionStop))
	agentd.WaitForBackgroundForTest()
	resumed := f.AsHuman().Resume(spawn.ConvID)
	assert.Equal(t, "resumed", resumed.Action,
		"authoritative server completion and attachment cleanup must admit the immediate successor: %s", resumed.Detail)
}

func TestOpenCodeStopRefusesRuntimeFromDifferentExecution(t *testing.T) {
	f := newFlow(t)
	const (
		conv  = "ses_opencode_replacement_fence"
		tmux  = "tmux-opencode-replacement-fence"
		label = "spwn-oc-replacement-fence"
	)
	_, server := openCodeTUICommandServer(t, f, tmux, false)
	defer server.Close()
	haveOpenCodeControlSession(t, f, conv, label, tmux, server.URL)
	t.Cleanup(agentd.SetVerifyOpenCodeRuntimeForStopTest(func(db.OpenCodeRuntime) bool { return true }))
	runtimeRow, err := db.GetOpenCodeRuntime(label)
	require.NoError(t, err)
	require.NotNil(t, runtimeRow)
	var boundary session.ExecutionBoundary
	require.NoError(t, json.Unmarshal([]byte(runtimeRow.ExecutionBoundaryJSON), &boundary))
	boundary.LaunchGeneration = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tampered, err := json.Marshal(boundary)
	require.NoError(t, err)
	runtimeRow.ExecutionBoundaryJSON = string(tampered)
	require.NoError(t, db.UpsertOpenCodeRuntime(*runtimeRow))
	called := false
	t.Cleanup(agentd.SetStopExactOpenCodeRuntimeForTest(func(db.OpenCodeRuntime, bool) (bool, error) {
		called = true
		return true, nil
	}))

	action := agentd.StopOneConvWithIntentForTest(conv, db.AgentExitActionStop)
	assert.Equal(t, "error", action)
	assert.False(t, called, "an unbound predecessor runtime must never reach native Stop")
}

func TestOpenCodeCompactWhileBusyReturnsRetryableFailureBeforeAPIOrKeys(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const (
		conv = "ses_opencode_busy_compact_api"
		tmux = "tmux-opencode-busy-api"
	)
	commands, server := openCodeTUICommandServer(t, f, tmux, false)
	defer server.Close()
	haveOpenCodeControlSession(t, f, conv, "spwn-oc-busy-api", tmux, server.URL)
	f.HaveMember("crew", conv)
	f.SetSessionStatus(conv, session.StatusWorking)

	res := f.AsHuman().Compact(conv)
	require.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Raw)
	select {
	case command := <-commands:
		t.Fatalf("busy OpenCode control was dispatched instead of deferred: %q", command)
	case <-time.After(50 * time.Millisecond):
	}
	assert.Empty(t, f.World.Tmux.Sent())
}

func TestOpenCodeCompactRechecksStatusImmediatelyBeforeAPI(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const (
		conv = "ses_opencode_compact_status_race"
		tmux = "tmux-opencode-status-race"
	)
	commands, server := openCodeTUICommandServer(t, f, tmux, false)
	defer server.Close()
	haveOpenCodeControlSession(t, f, conv, "spwn-oc-status-race", tmux, server.URL)
	f.HaveMember("crew", conv)
	t.Cleanup(agentd.SetBeforeOpenCodeTUICommandStatusCheckForTest(func() {
		f.SetSessionStatus(conv, session.StatusWorking)
	}))

	res := f.AsHuman().Compact(conv)
	require.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Raw)
	select {
	case command := <-commands:
		t.Fatalf("OpenCode control raced a fresh busy status: %q", command)
	case <-time.After(50 * time.Millisecond):
	}
	assert.Empty(t, f.World.Tmux.Sent())
}

func TestOpenCodeCompactFollowUpRejectedBeforeAPIOrKeys(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const (
		conv = "ses_opencode_compact_follow_up"
		tmux = "tmux-opencode-compact-follow-up"
	)
	commands, server := openCodeTUICommandServer(t, f, tmux, false)
	defer server.Close()
	haveOpenCodeControlSession(t, f, conv, "spwn-oc-compact-follow-up", tmux, server.URL)
	f.HaveMember("crew", conv)

	res := f.AsHuman().CompactWithFollowUp(conv, "continue after compact")
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Raw)
	select {
	case command := <-commands:
		t.Fatalf("unordered OpenCode compact pair dispatched command: %q", command)
	case <-time.After(50 * time.Millisecond):
	}
	assert.Empty(t, f.World.Tmux.Sent())
}

func TestOpenCodeCompactAPIFailureReturnsRetryableFailureWithoutKeyFallback(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const (
		conv = "ses_opencode_compact_api_failure"
		tmux = "tmux-opencode-compact-failure"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/global/health" {
			_, _ = w.Write([]byte(`{"healthy":true}`))
			return
		}
		http.Error(w, "not delivered", http.StatusInternalServerError)
	}))
	defer server.Close()
	haveOpenCodeControlSession(t, f, conv, "spwn-oc-compact-failure", tmux, server.URL)
	f.HaveMember("crew", conv)

	res := f.AsHuman().Compact(conv)
	require.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Raw)
	assert.Empty(t, f.World.Tmux.Sent(), "managed API failure must never fall back to keystrokes")
}

func TestOpenCodeUnreadReminderUsesPromptAPIWithoutKeys(t *testing.T) {
	f := newFlow(t)
	const (
		sender    = "oc-reminder-send-bbbb-cccc-000000000001"
		recipient = "ses_opencode_unread_reminder"
		label     = "spwn-oc-reminder"
		tmux      = "tmux-opencode-reminder"
	)
	prompts := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true}`))
		case "/session/" + recipient + "/prompt_async":
			var body struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Len(t, body.Parts, 1)
			prompts <- body.Parts[0].Text
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f.HaveGroup("crew")
	f.HaveMember("crew", sender)
	f.HaveMember("crew", recipient)
	haveOpenCodeControlSession(t, f, recipient, label, tmux, server.URL)
	rec := postMessage(t, f, sender, map[string]any{
		"to":   recipient,
		"body": "please review" + strings.Repeat(" reminder-fixture-padding", 100),
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	agentd.WaitForBackgroundForTest()
	first := receiveCommand(t, prompts)
	assert.Contains(t, first, "new agent message")

	st := agentd.NewUnreadReminderStateForTest()
	agentd.RunUnreadReminderTickForTest(time.Now().Add(11*time.Minute), st)
	assert.Contains(t, receiveCommand(t, prompts), "reminder —")
	assert.Empty(t, f.World.Tmux.Sent(), "OpenCode reminders must use prompt_async")
}

func TestClaudeAndCodexCompactKeySequencesRemainUnchanged(t *testing.T) {
	for _, harnessName := range []string{harness.DefaultName, harness.CodexName} {
		t.Run(harnessName, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("crew")
			conv := harnessName + "-control-sequence-000000000001"
			tmux := "tmux-" + harnessName + "-sequence"
			if harnessName == harness.CodexName {
				f.HaveAliveCodexSession(conv, "spwn-codex-sequence", tmux, f.TestCwd("codex-sequence"))
			} else {
				f.HaveAliveSession(conv, "spwn-claude-sequence", tmux, f.TestCwd("claude-sequence"))
			}
			f.HaveMember("crew", conv)

			res := f.AsHuman().Compact(conv)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Raw)
			assert.Equal(t, []string{"/compact", "Enter", "Enter"},
				sentTexts(f.World.Tmux.Sent()),
				"non-OpenCode send-keys must remain byte-for-byte unchanged")
		})
	}
}

func haveOpenCodeControlSession(
	t *testing.T,
	f *testharness.Flow,
	conv, label, tmux, serverURL string,
) {
	t.Helper()
	cwd := f.TestCwd(label)
	f.HaveAliveSession(conv, label, tmux, cwd)
	setSessionHarness(t, conv, harness.OpenCodeName)
	f.SetSessionStatus(conv, session.StatusIdle)
	row, err := db.FindSessionByConvID(conv)
	require.NoError(t, err)
	require.NotNil(t, row)
	boundary, err := json.Marshal(session.ExecutionBoundary{
		Version:          session.ExecutionBoundaryVersion,
		LaunchGeneration: row.ExecutionID.String(),
		Harness:          session.ExecutionHarness{Name: harness.OpenCodeName},
	})
	require.NoError(t, err)
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID:             label,
		ConvID:                conv,
		ServerURL:             serverURL,
		Password:              "test-password",
		PID:                   os.Getpid(),
		Cwd:                   cwd,
		ExecutionBoundaryJSON: string(boundary),
	}))
}

func openCodeTUICommandServer(
	t *testing.T,
	f *testharness.Flow,
	tmux string,
	exitOnCommand bool,
) (chan string, *httptest.Server) {
	t.Helper()
	commands := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		require.True(t, ok)
		assert.Equal(t, "opencode", user)
		assert.Equal(t, "test-password", pass)
		switch r.URL.Path {
		case "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true}`))
		case "/tui/publish":
			var body struct {
				Type       string `json:"type"`
				Properties struct {
					Command string `json:"command"`
				} `json:"properties"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, "tui.command.execute", body.Type)
			commands <- body.Properties.Command
			if exitOnCommand && body.Properties.Command == "app.exit" {
				f.World.Tmux.MarkOffline(tmux)
			}
			_, _ = w.Write([]byte("true"))
		default:
			http.NotFound(w, r)
		}
	}))
	return commands, server
}

func receiveCommand(t *testing.T, commands <-chan string) string {
	t.Helper()
	select {
	case command := <-commands:
		return command
	case <-time.After(10 * time.Second):
		t.Fatal("managed OpenCode TUI command was not received")
		return ""
	}
}

func sentTexts(sent []testharness.SentKey) []string {
	out := make([]string, len(sent))
	for i := range sent {
		out[i] = sent[i].Text
	}
	return out
}
