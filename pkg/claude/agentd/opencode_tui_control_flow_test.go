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

func TestOpenCodeSoftExitUsesManagedTUICommandAPIWithoutKeys(t *testing.T) {
	f := newFlow(t)
	const (
		conv = "ses_opencode_exit_api"
		tmux = "tmux-opencode-exit-api"
	)
	commands, server := openCodeTUICommandServer(t, f, tmux, true)
	defer server.Close()
	haveOpenCodeControlSession(t, f, conv, "spwn-oc-exit-api", tmux, server.URL)

	action := agentd.StopOneConvWithIntentForTest(conv, db.AgentExitActionStop)
	require.Equal(t, "soft_stopped", action)
	assert.Equal(t, "app.exit", receiveCommand(t, commands))
	assert.False(t, f.World.Tmux.IsAlive(tmux), "app.exit must close the attached TUI")
	assert.Empty(t, f.World.Tmux.Sent(), "OpenCode exit must not use tmux send-keys")
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
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: label,
		ConvID:    conv,
		ServerURL: serverURL,
		Password:  "test-password",
		PID:       os.Getpid(),
		Cwd:       cwd,
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
