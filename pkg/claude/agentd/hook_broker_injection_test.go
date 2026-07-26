package agentd_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// The hook broker opens a new path into the tmux send-keys injection
// sink: ApplyHook injects `/rename <title>` into the agent's pane after a
// /clear, and with TCL-754 the event that triggers it now arrives from
// INSIDE a sandbox while the injection happens on the host.
//
// The repo guardrail is that lifecycle command tokens stay compile-time
// constants and that any text reaching send-keys passes the charset gate
// where it can no longer be influenced. Both hold here, and this file is
// the standing proof:
//
//   - `/rename ` is a literal in restoreClearedTitle. Nothing composes it.
//   - The injected title is not a payload field at all. It is the name
//     db.RotateAgentConv carried off the OLD conversation, and it passes
//     isValidRenameTitle immediately before the send-keys call — i.e. at
//     the last possible moment, inside the daemon process, not at some
//     upstream point the payload could get behind.
//
// The tests below attack both halves: a hostile title reaching the gate
// through the agent's own transcript, and a hostile payload trying to
// reach send-keys directly.

const (
	injOldConv = "0a1d0000-1111-2222-3333-444444444444"
	injNewConv = "0e100000-1111-2222-3333-444444444444"
	injLabel   = "spwn-broker-inject"
	injTmux    = "tmux-broker-inject"

	injHookPID    = 7300
	injHarnessPID = 7301
	injBwrapPID   = 7302
	injPanePID    = 7303
)

// hostileTitle carries every shape that would matter if it ever reached a
// pane: an Enter keyname (tmux send-keys interprets those), a shell
// metacharacter chain, and a slash-command token.
const hostileTitle = "pwned\" Enter \"/exit; rm -rf ~ Enter /remote-control on"

func injectionProcTree(t *testing.T) int {
	t.Helper()
	t.Cleanup(agentd.SetProcTreeForTest(
		map[int]string{
			injHookPID:    "tclaude",
			injHarnessPID: "node",
			injBwrapPID:   "bwrap",
			injPanePID:    "sh",
		},
		map[int]int{
			injHookPID:    injHarnessPID,
			injHarnessPID: injBwrapPID,
			injBwrapPID:   injPanePID,
		},
	))
	return injHookPID
}

// TestHookBroker_HostileTitleCannotInjectKeys drives the real /clear
// identity migration through the broker with a hostile carried title, and
// asserts nothing resembling it ever reaches the pane.
//
// The title is planted where a real one comes from — the agent's own
// pending_name — so this exercises the actual production path rather than
// a hand-built call to the injector.
func TestHookBroker_HostileTitleCannotInjectKeys(t *testing.T) {
	f := newFlow(t)
	callerPID := injectionProcTree(t)

	haveLayerSession(t, f, injOldConv, injLabel, injTmux, injPanePID)
	f.HaveEnrolledAgent(injOldConv)
	f.HavePendingName(injOldConv, hostileTitle)

	// A SessionStart(source=clear) announcing the rotation, exactly as
	// Claude Code fires it after a /clear — the event that drives
	// migrateClearedIdentity and, with a valid title, the /rename inject.
	code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		Input: session.HookCallbackInput{
			ConvID:        injNewConv,
			HookEventName: "SessionStart",
			Source:        "clear",
			Cwd:           f.World.HomeDir,
		},
	})
	require.Equal(t, http.StatusOK, code, "the event itself is legitimate and must be applied")

	// Give any injection goroutine the same window a real one would get.
	time.Sleep(200 * time.Millisecond)

	for _, sent := range f.World.Tmux.Sent() {
		assert.NotContains(t, sent.Text, "rm -rf",
			"a hostile carried title must never reach send-keys: %+v", sent)
		assert.NotContains(t, sent.Text, "/exit",
			"a hostile carried title must never smuggle a lifecycle command: %+v", sent)
		assert.NotContains(t, sent.Text, "/remote-control",
			"a hostile carried title must never smuggle a lifecycle command: %+v", sent)
		assert.NotContains(t, sent.Text, "pwned",
			"the rejected title must be dropped whole, not partially typed: %+v", sent)
	}
}

// TestHookBroker_ValidTitleStillInjects is the other half of the same
// property, and the reason the test above is not vacuously true: the
// injection path IS reachable through the broker, so a gate that silently
// disabled it would look identical to a gate that works.
func TestHookBroker_ValidTitleStillInjects(t *testing.T) {
	f := newFlow(t)
	callerPID := injectionProcTree(t)

	haveLayerSession(t, f, injOldConv, injLabel, injTmux, injPanePID)
	f.HaveEnrolledAgent(injOldConv)
	f.HavePendingName(injOldConv, "perfectly-ordinary-name")

	code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		Input: session.HookCallbackInput{
			ConvID:        injNewConv,
			HookEventName: "SessionStart",
			Source:        "clear",
			Cwd:           f.World.HomeDir,
		},
	})
	require.Equal(t, http.StatusOK, code)

	f.AssertSentContains(injTmux+":0.0", "/rename perfectly-ordinary-name", 2*time.Second)

	// And the migration itself committed — the broker really did drive the
	// production /clear path, not a truncated version of it.
	succ, err := db.GetConvSuccessor(injOldConv)
	require.NoError(t, err)
	assert.Equal(t, injNewConv, succ, "the brokered /clear must record the succession edge")
}

// TestHookBroker_PayloadTextNeverReachesThePane checks the direction the
// title test cannot: a payload whose every free-text field is hostile.
// None of them feed send-keys — they are stored and rendered — and this
// pins that, so a future change that starts typing a payload field into a
// pane fails here rather than in production.
func TestHookBroker_PayloadTextNeverReachesThePane(t *testing.T) {
	f := newFlow(t)
	callerPID := injectionProcTree(t)

	haveLayerSession(t, f, injOldConv, injLabel, injTmux, injPanePID)

	const marker = "INJECTIONCANARY"
	hostile := marker + `" Enter "/exit`

	for _, event := range []string{"UserPromptSubmit", "Notification", "Stop", "PostToolUse"} {
		code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
			Input: session.HookCallbackInput{
				ConvID:               injOldConv,
				HookEventName:        event,
				Cwd:                  f.World.HomeDir,
				Prompt:               hostile,
				Message:              hostile,
				LastAssistantMessage: hostile,
				ErrorMessage:         hostile,
				ToolName:             hostile,
				NotificationType:     hostile,
			},
		})
		require.Equal(t, http.StatusOK, code, "brokered %s", event)
	}

	time.Sleep(200 * time.Millisecond)
	for _, sent := range f.World.Tmux.Sent() {
		assert.False(t, strings.Contains(sent.Text, marker),
			"no hook payload field may reach send-keys: %+v", sent)
	}
}
