package session

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	paneID               string
	deadOutput           string
	failSetHook          bool
	noNativePaneDied     bool
	failKillPaneCount    int
	failKillSessionCount int
	calls                [][]string
}

func (f *exitCallbackTmux) Command(args ...string) *exec.Cmd {
	f.calls = append(f.calls, slices.Clone(args))
	if len(args) > 0 && args[0] == "set-hook" {
		if f.failSetHook {
			return exec.Command("false")
		}
		return exec.Command("true")
	}
	if len(args) > 0 && args[0] == "show-hooks" {
		if f.noNativePaneDied {
			return exec.Command("true")
		}
		return exec.Command("sh", "-c", "printf '%s' pane-died")
	}
	if len(args) > 0 && args[0] == "display-message" {
		format := args[len(args)-1]
		out := f.deadOutput
		if format == "#{pane_id}" {
			out = f.paneID
		}
		return exec.Command("sh", "-c", "printf '%s' \"$1\"", "sh", out)
	}
	if len(args) > 0 && args[0] == "kill-pane" && f.failKillPaneCount > 0 {
		f.failKillPaneCount--
		return exec.Command("false")
	}
	if len(args) > 0 && args[0] == "kill-session" && f.failKillSessionCount > 0 {
		f.failKillSessionCount--
		return exec.Command("false")
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
	const generation = "11111111111111111111111111111111"
	require.NoError(t, SaveSessionStateForLaunch(&SessionState{
		ID: "spwn-guard", TmuxSession: "tmux-guard", ConvID: "conv-guard",
		Status: StatusIdle, Created: time.Now(),
	}, generation, db.SessionExitGatePending))

	guard, err := newExitLaunchGuard("spwn-guard", "tmux-guard", generation)
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
	guard.bind()
	require.NoError(t, guard.release())
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
	assert.Contains(t, hook, "pane-died")
	assert.Contains(t, hook[len(hook)-1], "while [ \"$tclaude_exit_i\" -lt 3 ]",
		"the callback watchdog cleanup is bounded")
	assert.Contains(t, hook[len(hook)-1], "kill-session",
		"repeated kill-pane failure has an exact-session fallback")
	d, err := db.Open()
	require.NoError(t, err)
	var durableHash, gateState string
	require.NoError(t, d.QueryRow(`SELECT exit_callback_token_hash, exit_launch_gate_state
		FROM sessions WHERE id = ?`, "spwn-guard").Scan(&durableHash, &gateState))
	assert.Equal(t, guard.tokenHash, durableHash)
	assert.NotEqual(t, plainToken, durableHash, "durable state contains only the token hash")
	assert.Equal(t, db.SessionExitGateReleased, gateState,
		"runtime release state is durable before the pane crosses the file gate")
}

func TestExitLaunchGuard_FileReleaseFailureRestoresPreHarnessState(t *testing.T) {
	fake := &exitCallbackTmux{paneID: "%8"}
	setupExitCallbackTest(t, fake)
	const generation = "88888888888888888888888888888888"
	require.NoError(t, SaveSessionStateForLaunch(&SessionState{
		ID: "spwn-release-fail", TmuxSession: "tmux-release-fail", ConvID: "conv-release-fail",
		Status: StatusIdle, Created: time.Now(),
	}, generation, db.SessionExitGatePending))
	guard, err := newExitLaunchGuard("spwn-release-fail", "tmux-release-fail", generation)
	require.NoError(t, err)
	originalBarrier := guard.barrierPath
	defer func() {
		guard.barrierPath = originalBarrier
		guard.abort()
	}()
	guard.armPaneHook()
	guard.bind()
	guard.barrierPath = filepath.Join(t.TempDir(), "missing", "barrier")
	require.Error(t, guard.release())

	identity, err := db.GetSessionExitLaunchIdentity("spwn-release-fail")
	require.NoError(t, err)
	assert.Equal(t, db.SessionExitGatePending, identity.GateState,
		"a pane that never crossed the file gate remains a pre-harness launch")
}

func TestRunExitCallback_VerifiesDeadPaneAndRejectsReplay(t *testing.T) {
	fake := &exitCallbackTmux{
		paneID: "%9", deadOutput: "tmux-callback|%9|1||TERM|11111111111111111111111111111111",
	}
	setupExitCallbackTest(t, fake)
	require.NoError(t, SaveSessionState(&SessionState{
		ID: "spwn-callback", TmuxSession: "tmux-callback", ConvID: "conv-callback",
		Status: StatusWorking, Created: time.Now(),
	}))
	const generation = "11111111111111111111111111111111"
	const token = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hash := sha256.Sum256([]byte(token))
	require.NoError(t, db.SetSessionExitLaunchGeneration("spwn-callback", generation))
	require.NoError(t, db.SetSessionExitLaunchBinding(
		"spwn-callback", generation, hex.EncodeToString(hash[:]), "%9"))
	require.NoError(t, db.MarkSessionExitLaunchReleased("spwn-callback", generation))
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
		paneID: "%11", deadOutput: "tmux-real|%11|0|7||22222222222222222222222222222222",
	}
	setupExitCallbackTest(t, fake)
	require.NoError(t, SaveSessionState(&SessionState{
		ID: "spwn-hostile", TmuxSession: "tmux-real", ConvID: "conv-hostile",
		Status: StatusWorking, Created: time.Now(),
	}))
	const generation = "22222222222222222222222222222222"
	const token = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	hash := sha256.Sum256([]byte(token))
	require.NoError(t, db.SetSessionExitLaunchGeneration("spwn-hostile", generation))
	require.NoError(t, db.SetSessionExitLaunchBinding(
		"spwn-hostile", generation, hex.EncodeToString(hash[:]), "%11"))

	base := exitCallbackParams{
		SessionID: "spwn-hostile", TmuxSession: "tmux-real", PaneID: "%11",
		Generation: generation, Token: token, ExitCode: "7",
	}
	err := runExitCallback(base)
	require.ErrorIs(t, err, db.ErrExitCallbackRejected, "a still-live pane cannot forge its own exit")

	fake.deadOutput = "tmux-other|%11|1|7||22222222222222222222222222222222"
	err = runExitCallback(base)
	require.ErrorIs(t, err, db.ErrExitCallbackRejected, "tmux session attribution must match")
	fake.deadOutput = "tmux-real|%11|1|9||22222222222222222222222222222222"
	err = runExitCallback(base)
	require.ErrorIs(t, err, db.ErrExitCallbackRejected, "callback values must match tmux formats")

	n, err := db.CountAuditLog(db.AuditLogFilter{Verb: db.AuditVerbAgentExit})
	require.NoError(t, err)
	assert.Zero(t, n)
	for _, call := range fake.calls {
		assert.NotEqual(t, "kill-pane", call[0], "rejected callback leaves evidence for reaper/watchdog")
	}
}

func TestRunExitCallback_CleanupFailureKeepsRecordedEvidence(t *testing.T) {
	fake := &exitCallbackTmux{
		paneID: "%18", deadOutput: "tmux-cleanup-fail|%18|1|9||66666666666666666666666666666666",
		failKillPaneCount: 3, failKillSessionCount: 1,
	}
	setupExitCallbackTest(t, fake)
	require.NoError(t, SaveSessionState(&SessionState{
		ID: "spwn-cleanup-fail", TmuxSession: "tmux-cleanup-fail",
		ConvID: "conv-cleanup-fail", Status: StatusWorking, Created: time.Now(),
	}))
	const generation = "66666666666666666666666666666666"
	const token = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	hash := sha256.Sum256([]byte(token))
	require.NoError(t, db.SetSessionExitLaunchGeneration("spwn-cleanup-fail", generation))
	require.NoError(t, db.SetSessionExitLaunchBinding(
		"spwn-cleanup-fail", generation, hex.EncodeToString(hash[:]), "%18"))
	require.NoError(t, db.MarkSessionExitLaunchReleased("spwn-cleanup-fail", generation))

	err := runExitCallback(exitCallbackParams{
		SessionID: "spwn-cleanup-fail", TmuxSession: "tmux-cleanup-fail", PaneID: "%18",
		Generation: generation, Token: token, ExitCode: "9",
	})
	require.ErrorContains(t, err, "clean recorded dead pane")
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: db.AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, rows, 1, "cleanup failure must not roll back the record-first audit event")
	assert.Equal(t, 9, *rows[0].ExitCode)
	var killPane, killSession int
	for _, call := range fake.calls {
		switch call[0] {
		case "kill-pane":
			killPane++
		case "kill-session":
			killSession++
		}
	}
	assert.Equal(t, 3, killPane)
	assert.Equal(t, 1, killSession, "cleanup falls back once to the exact managed session")
}

func TestRunExitCallback_AuditFailureLeavesPaneForBoundedRecovery(t *testing.T) {
	fake := &exitCallbackTmux{
		paneID: "%19", deadOutput: "tmux-audit-fail|%19|1|17||77777777777777777777777777777777",
	}
	setupExitCallbackTest(t, fake)
	require.NoError(t, SaveSessionState(&SessionState{
		ID: "spwn-audit-fail", TmuxSession: "tmux-audit-fail",
		ConvID: "conv-audit-fail", Status: StatusWorking, Created: time.Now(),
	}))
	const generation = "77777777777777777777777777777777"
	const token = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	hash := sha256.Sum256([]byte(token))
	require.NoError(t, db.SetSessionExitLaunchGeneration("spwn-audit-fail", generation))
	require.NoError(t, db.SetSessionExitLaunchBinding(
		"spwn-audit-fail", generation, hex.EncodeToString(hash[:]), "%19"))
	require.NoError(t, db.MarkSessionExitLaunchReleased("spwn-audit-fail", generation))
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`DROP TABLE audit_log`)
	require.NoError(t, err)

	err = runExitCallback(exitCallbackParams{
		SessionID: "spwn-audit-fail", TmuxSession: "tmux-audit-fail", PaneID: "%19",
		Generation: generation, Token: token, ExitCode: "17",
	})
	require.ErrorContains(t, err, "record managed pane exit")
	for _, call := range fake.calls {
		assert.NotEqual(t, "kill-pane", call[0], "record failure preserves exact evidence for reaper/watchdog recovery")
	}
	var usedAt string
	require.NoError(t, d.QueryRow(`SELECT COALESCE(exit_callback_used_at, '') FROM sessions WHERE id = ?`,
		"spwn-audit-fail").Scan(&usedAt))
	assert.Empty(t, usedAt, "failed audit transaction must not consume callback authority")
}

func TestExitLaunchGuard_UnsupportedTmuxFallsBackWithoutBlockingLaunch(t *testing.T) {
	fake := &exitCallbackTmux{paneID: "%15", noNativePaneDied: true}
	setupExitCallbackTest(t, fake)
	const generation = "55555555555555555555555555555555"
	require.NoError(t, SaveSessionStateForLaunch(&SessionState{
		ID: "spwn-old-tmux", TmuxSession: "tmux-old", ConvID: "conv-old",
		Status: StatusWorking, Created: time.Now(),
	}, generation, db.SessionExitGatePending))
	guard, err := newExitLaunchGuard("spwn-old-tmux", "tmux-old", generation)
	require.NoError(t, err)
	defer guard.abort()
	guard.armPaneHook()
	assert.False(t, guard.callbackEnabled)
	for _, call := range fake.calls {
		assert.NotEqual(t, "set-hook", call[0],
			"empty native show-hooks output must degrade before arbitrary custom-hook acceptance")
	}
	guard.bind()
	require.NoError(t, guard.release(), "unsupported hook degrades to reaper, not launch failure")
	raw, err := os.ReadFile(guard.barrierPath)
	require.NoError(t, err)
	assert.Equal(t, "go", string(raw))

	recorded, err := db.RecordAgentExitObservation(db.AgentExitObservation{
		SessionID: "spwn-old-tmux", Observer: db.AgentExitObserverReconcile,
		CauseKind: db.AgentExitCauseDisappeared, ExpectedGeneration: guard.generation,
	})
	require.NoError(t, err)
	assert.True(t, recorded.Inserted)
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: db.AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, db.AgentExitCauseDisappeared, rows[0].CauseKind)
	assert.True(t, strings.Contains(rows[0].Detail, "exit_code=unavailable"))
}

func TestExitLaunchGuard_NativeHookInstallFailureDisarmsRetainedPane(t *testing.T) {
	fake := &exitCallbackTmux{paneID: "%20", failSetHook: true}
	setupExitCallbackTest(t, fake)
	const generation = "99999999999999999999999999999999"
	require.NoError(t, SaveSessionStateForLaunch(&SessionState{
		ID: "spwn-hook-fail", TmuxSession: "tmux-hook-fail", ConvID: "conv-hook-fail",
		Status: StatusIdle, Created: time.Now(),
	}, generation, db.SessionExitGatePending))
	guard, err := newExitLaunchGuard("spwn-hook-fail", "tmux-hook-fail", generation)
	require.NoError(t, err)
	defer guard.abort()
	guard.armPaneHook()
	assert.False(t, guard.callbackEnabled)
	var remainOff bool
	for _, call := range fake.calls {
		if slices.Contains(call, "remain-on-exit") && slices.Contains(call, "off") {
			remainOff = true
		}
	}
	assert.True(t, remainOff, "partial arm failure must not retain an unauditable dead pane")
}

func TestExitLaunchGuard_AbortAndStartupRemoveBarrierArtifacts(t *testing.T) {
	fake := &exitCallbackTmux{paneID: "%16"}
	setupExitCallbackTest(t, fake)

	aborted, err := newExitLaunchGuard("spwn-abort", "tmux-abort", "33333333333333333333333333333333")
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

	next, err := newExitLaunchGuard("spwn-next", "tmux-next", "44444444444444444444444444444444")
	require.NoError(t, err)
	t.Cleanup(next.abort)
	_, err = os.Stat(stalePath)
	require.ErrorIs(t, err, os.ErrNotExist, "a later launch cleans artifacts left by a crashed parent")
}

func TestExitLaunchGeneration_RNGDegradationStillResetsPredecessorAuthority(t *testing.T) {
	fake := &exitCallbackTmux{paneID: "%17"}
	setupExitCallbackTest(t, fake)
	const predecessor = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	require.NoError(t, SaveSessionStateForLaunch(&SessionState{
		ID: "spwn-degraded", TmuxSession: "tmux-degraded", ConvID: "conv-degraded",
		Status: StatusWorking, Created: time.Now(),
	}, predecessor, db.SessionExitGatePending))
	require.NoError(t, db.SetSessionExitLaunchBinding("spwn-degraded", predecessor,
		strings.Repeat("b", 64), "%17"))
	_, err := db.SetSessionExitIntent("spwn-degraded", db.AgentExitActionStop,
		"evt_1234567890abcdef12345678", time.Now())
	require.NoError(t, err)

	previousRead := exitRandomRead
	exitRandomRead = func([]byte) (int, error) { return 0, errors.New("rng unavailable") }
	t.Cleanup(func() { exitRandomRead = previousRead })
	generation := newExitLaunchGeneration("spwn-degraded", "tmux-degraded")
	require.NotEqual(t, predecessor, generation)
	require.True(t, validCallbackHex(generation, 32))
	require.NoError(t, SaveSessionStateForLaunch(&SessionState{
		ID: "spwn-degraded", TmuxSession: "tmux-degraded", ConvID: "conv-degraded",
		Status: StatusWorking, Created: time.Now(),
	}, generation, db.SessionExitGateUngated))
	_, err = newExitLaunchGuard("spwn-degraded", "tmux-degraded", generation)
	require.Error(t, err, "private token setup degrades after fresh authority is already durable")

	d, err := db.Open()
	require.NoError(t, err)
	var gotGeneration, tokenHash, intent, intentGeneration string
	require.NoError(t, d.QueryRow(`SELECT exit_callback_generation,
		exit_callback_token_hash, exit_intent, exit_intent_generation
		FROM sessions WHERE id = 'spwn-degraded'`).Scan(
		&gotGeneration, &tokenHash, &intent, &intentGeneration))
	assert.Equal(t, generation, gotGeneration)
	assert.Empty(t, tokenHash)
	assert.Empty(t, intent)
	assert.Empty(t, intentGeneration)
}
