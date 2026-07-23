package agentd_test

import (
	"encoding/json"
	"fmt"
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
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TestOpenCodeInlineMessageUsesManagedPromptAPI is the TCL-699 production-path
// regression. The message enters through POST /v1/messages, is formatted and
// claimed by the normal async queue, and reaches the server-authoritative
// OpenCode conversation through prompt_async. No untrusted message bytes may
// be typed into the attach pane.
func TestOpenCodeInlineMessageUsesManagedPromptAPI(t *testing.T) {
	f := newFlow(t)
	const (
		sender    = "oc-msg-send-bbbb-cccc-000000000001"
		recipient = "ses_opencode_message_delivery"
		label     = "spwn-oc-msg-recv"
		tmux      = "tclaude-oc-msg-recv"
		password  = "private-password"
	)
	cwd := f.TestCwd("opencode-message")

	prompts := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		require.True(t, ok)
		assert.Equal(t, "opencode", user)
		assert.Equal(t, password, pass)
		switch r.URL.Path {
		case "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true}`))
		case "/session/" + recipient + "/prompt_async":
			assert.Equal(t, cwd, r.URL.Query().Get("directory"))
			var body struct {
				Parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"parts"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Len(t, body.Parts, 1)
			assert.Equal(t, "text", body.Parts[0].Type)
			prompts <- body.Parts[0].Text
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f.HaveGroup("team")
	f.HaveConvWithTitle(sender, "sender")
	f.HaveConvWithTitle(recipient, "opencode-worker")
	f.HaveMember("team", sender)
	f.HaveMember("team", recipient)
	f.HaveAliveSession(recipient, label, tmux, cwd)
	setSessionHarness(t, recipient, harness.OpenCodeName)
	require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
		SessionID: label,
		ConvID:    recipient,
		ServerURL: server.URL,
		Password:  password,
		PID:       os.Getpid(),
		Cwd:       cwd,
	}))

	rec := postMessage(t, f, sender, map[string]any{
		"to": recipient, "subject": "review", "body": "inspect the failing test",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	sent := decodeSend(t, rec)
	require.True(t, sent.Queued)
	require.NotZero(t, sent.ID)
	agentd.WaitForBackgroundForTest()

	var prompt string
	select {
	case prompt = <-prompts:
	case <-time.After(10 * time.Second):
		t.Fatal("OpenCode prompt_async did not receive the queued message")
	}
	assert.Contains(t, prompt, fmt.Sprintf("[system: new agent message #%d", sent.ID))
	assert.Contains(t, prompt, "; delivery: inline")
	assert.Contains(t, prompt, "; subject: review]")
	assert.True(t, strings.HasSuffix(prompt, "] inspect the failing test"), prompt)
	assertNoPaneMessageInjection(t, f, tmux+":0.0", sent.ID)

	stored, err := db.GetAgentMessage(sent.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.False(t, stored.DeliveredAt.IsZero())
	assert.False(t, stored.ReadAt.IsZero(),
		"the inline archival row is consumed only after prompt_async succeeds")
}

// TestOpenCodeMessageWithoutRuntimeStaysQueued proves the failure mode is
// retryable. OpenCode must never fall back to tmux for untrusted message
// content when its managed server is absent.
func TestOpenCodeMessageWithoutRuntimeStaysQueued(t *testing.T) {
	f := newFlow(t)
	const (
		sender    = "oc-miss-send-bbbb-cccc-000000000001"
		recipient = "ses_opencode_missing_runtime"
		tmux      = "tclaude-oc-miss-recv"
	)
	f.HaveGroup("team")
	f.HaveMember("team", sender)
	f.HaveMember("team", recipient)
	f.HaveAliveSession(recipient, "spwn-oc-miss-recv", tmux, f.TestCwd("opencode-missing"))
	setSessionHarness(t, recipient, harness.OpenCodeName)

	rec := postMessage(t, f, sender, map[string]any{
		"to": recipient, "body": "preserve this message",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	sent := decodeSend(t, rec)
	agentd.WaitForBackgroundForTest()

	stored, err := db.GetAgentMessage(sent.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.True(t, stored.DeliveredAt.IsZero(), "failed API delivery remains queued")
	assert.True(t, stored.NudgeClaimedAt.IsZero(), "failed API delivery releases its claim")
	assert.Equal(t, 1, stored.NudgeAttempts, "the existing retry policy records the attempt")
	assert.True(t, stored.ReadAt.IsZero(), "failed inline delivery does not consume the inbox row")
	assertNoPaneMessageInjection(t, f, tmux+":0.0", sent.ID)
}

// TestClaudeAndCodexInlineMessagesKeepTmuxDelivery pins the non-OpenCode side
// of the dispatch. Their existing REPL panes still receive the exact framed
// inline content through send-keys.
func TestClaudeAndCodexInlineMessagesKeepTmuxDelivery(t *testing.T) {
	for _, harnessName := range []string{harness.DefaultName, harness.CodexName} {
		t.Run(harnessName, func(t *testing.T) {
			f := newFlow(t)
			sender := harnessName + "-send-bbbb-cccc-000000000001"
			recipient := harnessName + "-recv-bbbb-cccc-000000000002"
			label := "spwn-" + harnessName + "-recv"
			tmux := "tclaude-" + harnessName + "-recv"
			cwd := f.TestCwd(harnessName + "-message")
			f.HaveGroup("team")
			f.HaveMember("team", sender)
			f.HaveMember("team", recipient)
			if harnessName == harness.CodexName {
				f.HaveAliveCodexSession(recipient, label, tmux, cwd)
			} else {
				f.HaveAliveSession(recipient, label, tmux, cwd)
				setSessionHarness(t, recipient, harness.DefaultName)
			}

			rec := postMessage(t, f, sender, map[string]any{
				"to": recipient, "subject": "unchanged", "body": "existing pane delivery",
			})
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
			sent := decodeSend(t, rec)
			agentd.WaitForBackgroundForTest()

			nudge := paneMessageText(t, f, tmux+":0.0", sent.ID)
			assert.Contains(t, nudge, "; delivery: inline")
			assert.Contains(t, nudge, "; subject: unchanged]")
			assert.True(t, strings.HasSuffix(nudge, "] existing pane delivery"), nudge)
		})
	}
}

func setSessionHarness(t *testing.T, convID, harnessName string) {
	t.Helper()
	rows, err := db.FindSessionsByConvID(convID)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	for _, row := range rows {
		row.Harness = harnessName
		require.NoError(t, db.SaveSession(row))
	}
}

func assertNoPaneMessageInjection(t *testing.T, f *testharness.Flow, target string, msgID int64) {
	t.Helper()
	needle := fmt.Sprintf("new agent message #%d", msgID)
	for _, sent := range f.World.Tmux.Sent() {
		if sent.Target == target && strings.Contains(sent.Text, needle) {
			t.Fatalf("message content reached OpenCode pane through send-keys: %+v", sent)
		}
	}
}

func paneMessageText(t *testing.T, f *testharness.Flow, target string, msgID int64) string {
	t.Helper()
	needle := fmt.Sprintf("new agent message #%d", msgID)
	for _, sent := range f.World.Tmux.Sent() {
		if sent.Target == target && strings.Contains(sent.Text, needle) {
			return sent.Text
		}
	}
	t.Fatalf("no send-keys to %s contained %q; got %+v", target, needle, f.World.Tmux.Sent())
	return ""
}
