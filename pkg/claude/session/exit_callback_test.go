package session

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type exitCallbackTmux struct {
	paneID      string
	deadOutput  string
	failSetHook bool
	calls       [][]string
}

func (f *exitCallbackTmux) Command(args ...string) *exec.Cmd {
	f.calls = append(f.calls, slices.Clone(args))
	if len(args) > 0 && args[0] == "set-hook" {
		if f.failSetHook {
			return exec.Command("false")
		}
		return exec.Command("true")
	}
	if len(args) > 0 && args[0] == "display-message" {
		format := args[len(args)-1]
		out := f.deadOutput
		if format == "#{pane_id}" {
			out = f.paneID
		}
		return exec.Command("sh", "-c", "printf '%s' \"$1\"", "sh", out)
	}
	return exec.Command("true")
}

func (f *exitCallbackTmux) ListSessions() (map[string]struct{}, error) { return nil, nil }

func setupExitCallbackTest(t *testing.T, fake *exitCallbackTmux) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()
	prev := clcommon.Default
	clcommon.Default = fake
	t.Cleanup(func() { clcommon.Default = prev })
}

func TestExitLaunchGuard_PaneLocalHookThenDurableBindingBeforeRelease(t *testing.T) {
	fake := &exitCallbackTmux{paneID: "%7"}
	setupExitCallbackTest(t, fake)
	require.NoError(t, SaveSessionState(&SessionState{
		ID: "spwn-guard", TmuxSession: "tmux-guard", ConvID: "conv-guard",
		Status: StatusIdle, Created: time.Now(),
	}))

	guard, err := newExitLaunchGuard("spwn-guard", "tmux-guard")
	require.NoError(t, err)
	defer guard.abort()
	plainToken := guard.token
	wrapped := guard.wrap("exec harness")
	assert.Contains(t, wrapped, "tclaude_exit_gate")
	assert.Contains(t, wrapped, "-lt 3000", "barrier wait must be bounded")
	assert.NotContains(t, wrapped, plainToken, "callback secret is not exported to the pane command/environment")

	guard.armPaneHook()
	require.True(t, guard.callbackEnabled)
	assert.Empty(t, guard.token, "parent drops its plaintext token once the local hook owns the callback command")
	require.NoError(t, guard.bindAndRelease())
	raw, err := os.ReadFile(guard.barrierPath)
	require.NoError(t, err)
	assert.Equal(t, "go", string(raw), "release follows hook installation + DB binding")

	var hook []string
	for _, call := range fake.calls {
		if len(call) > 0 && call[0] == "set-hook" {
			hook = call
		}
	}
	require.NotEmpty(t, hook)
	assert.Contains(t, hook, "-p", "hook is pane-local")
	assert.NotContains(t, hook, "-g", "hook must never be global")
	assert.Contains(t, hook, "=tmux-guard:0.0", "hook target is exact launch pane")
	assert.Contains(t, hook, "pane-exited")
	d, err := db.Open()
	require.NoError(t, err)
	var durableHash string
	require.NoError(t, d.QueryRow(`SELECT exit_callback_token_hash FROM sessions WHERE id = ?`, "spwn-guard").Scan(&durableHash))
	assert.Equal(t, guard.tokenHash, durableHash)
	assert.NotEqual(t, plainToken, durableHash, "durable state contains only the token hash")
}

func TestRunExitCallback_VerifiesDeadPaneAndRejectsReplay(t *testing.T) {
	fake := &exitCallbackTmux{
		paneID: "%9", deadOutput: "tmux-callback|%9|1||TERM",
	}
	setupExitCallbackTest(t, fake)
	require.NoError(t, SaveSessionState(&SessionState{
		ID: "spwn-callback", TmuxSession: "tmux-callback", ConvID: "conv-callback",
		Status: StatusWorking, Created: time.Now(),
	}))
	const generation = "11111111111111111111111111111111"
	const token = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hash := sha256.Sum256([]byte(token))
	require.NoError(t, db.SetSessionExitLaunchBinding(
		"spwn-callback", generation, hex.EncodeToString(hash[:]), "%9"))
	p := exitCallbackParams{
		SessionID: "spwn-callback", TmuxSession: "tmux-callback", PaneID: "%9",
		Generation: generation, Token: token, Signal: "TERM",
	}
	require.NoError(t, runExitCallback(p))
	err := runExitCallback(p)
	require.ErrorIs(t, err, db.ErrExitCallbackRejected, "credential is one-time/replay-safe")

	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: db.AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, db.AgentExitCauseSignal, rows[0].CauseKind)
	assert.Equal(t, "TERM", rows[0].Signal)
}

func TestRunExitCallback_RejectsLiveForgedAndMismatchedEvidence(t *testing.T) {
	fake := &exitCallbackTmux{
		paneID: "%11", deadOutput: "tmux-real|%11|0|7|",
	}
	setupExitCallbackTest(t, fake)
	require.NoError(t, SaveSessionState(&SessionState{
		ID: "spwn-hostile", TmuxSession: "tmux-real", ConvID: "conv-hostile",
		Status: StatusWorking, Created: time.Now(),
	}))
	const generation = "22222222222222222222222222222222"
	const token = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	hash := sha256.Sum256([]byte(token))
	require.NoError(t, db.SetSessionExitLaunchBinding(
		"spwn-hostile", generation, hex.EncodeToString(hash[:]), "%11"))

	base := exitCallbackParams{
		SessionID: "spwn-hostile", TmuxSession: "tmux-real", PaneID: "%11",
		Generation: generation, Token: token, ExitCode: "7",
	}
	err := runExitCallback(base)
	require.ErrorIs(t, err, db.ErrExitCallbackRejected, "a still-live pane cannot forge its own exit")

	fake.deadOutput = "tmux-other|%11|1|7|"
	err = runExitCallback(base)
	require.ErrorIs(t, err, db.ErrExitCallbackRejected, "tmux session attribution must match")
	fake.deadOutput = "tmux-real|%11|1|9|"
	err = runExitCallback(base)
	require.ErrorIs(t, err, db.ErrExitCallbackRejected, "callback values must match tmux formats")

	n, err := db.CountAuditLog(db.AuditLogFilter{Verb: db.AuditVerbAgentExit})
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestExitLaunchGuard_UnsupportedTmuxFallsBackWithoutBlockingLaunch(t *testing.T) {
	fake := &exitCallbackTmux{paneID: "%15", failSetHook: true}
	setupExitCallbackTest(t, fake)
	require.NoError(t, SaveSessionState(&SessionState{
		ID: "spwn-old-tmux", TmuxSession: "tmux-old", ConvID: "conv-old",
		Status: StatusWorking, Created: time.Now(),
	}))
	guard, err := newExitLaunchGuard("spwn-old-tmux", "tmux-old")
	require.NoError(t, err)
	defer guard.abort()
	guard.armPaneHook()
	assert.False(t, guard.callbackEnabled)
	require.NoError(t, guard.bindAndRelease(), "unsupported hook degrades to reaper, not launch failure")
	raw, err := os.ReadFile(guard.barrierPath)
	require.NoError(t, err)
	assert.Equal(t, "go", string(raw))

	recorded, err := db.RecordAgentExitObservation(db.AgentExitObservation{
		SessionID: "spwn-old-tmux", Observer: db.AgentExitObserverReconcile,
		CauseKind: db.AgentExitCauseDisappeared,
	})
	require.NoError(t, err)
	assert.True(t, recorded.Inserted)
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: db.AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, db.AgentExitCauseDisappeared, rows[0].CauseKind)
	assert.True(t, strings.Contains(rows[0].Detail, "exit_code=unavailable"))
}

func TestExitLaunchGuard_AbortAndStartupRemoveBarrierArtifacts(t *testing.T) {
	fake := &exitCallbackTmux{paneID: "%16"}
	setupExitCallbackTest(t, fake)

	aborted, err := newExitLaunchGuard("spwn-abort", "tmux-abort")
	require.NoError(t, err)
	abortPath := aborted.barrierPath
	aborted.abort()
	_, err = os.Stat(abortPath)
	require.ErrorIs(t, err, os.ErrNotExist, "cancelled launch must not leave a credential barrier")

	dir := filepath.Dir(abortPath)
	stalePath := filepath.Join(dir, exitLaunchArtifactPrefix+"stale")
	require.NoError(t, os.WriteFile(stalePath, []byte("pending"), 0o600))
	old := time.Now().Add(-exitLaunchStaleAfter - time.Second)
	require.NoError(t, os.Chtimes(stalePath, old, old))

	next, err := newExitLaunchGuard("spwn-next", "tmux-next")
	require.NoError(t, err)
	t.Cleanup(next.abort)
	_, err = os.Stat(stalePath)
	require.ErrorIs(t, err, os.ErrNotExist, "a later launch cleans artifacts left by a crashed parent")
}
