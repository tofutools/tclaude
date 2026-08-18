package agentd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/statusbar"
)

// A launch row is written before tmux starts, with pid=0, and gains its pane
// pid immediately after launch. Pin the gap itself: an old row owns the newly
// reused pane pid, so the ordinary ancestry resolver returns that row because
// the real one is not a pid candidate yet. The live tmux pane proof must
// recover the new row without treating its caller-supplied id as authority.
func TestClaimedLivePaneSessionRowRepairsStartupPIDGap(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(ResetBrokerLimiterForTest())

	const (
		callerPID  = 9101
		harnessPID = 9090
		bwrapPID   = 9080
		panePID    = 9070
		oldLabel   = "spwn-old-pid-owner"
		newLabel   = "spwn-new-starting"
		generation = "launch-new-starting"
	)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: oldLabel, PID: panePID, ConvID: "old-conv", TmuxSession: "tmux-old",
		Harness: harness.CopilotName, Status: "working",
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
	}))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: newLabel, PID: 0, ConvID: "new-conv", TmuxSession: "tmux-new",
		Harness: harness.CopilotName, Status: "idle",
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		ExitLaunchGeneration:  generation,
	}))
	// Make the old row unquestionably the ordinary resolver's incumbent.
	stampSessionUpdatedAt(t, oldLabel, time.Now())
	stampSessionUpdatedAt(t, newLabel, time.Now().Add(-time.Second))

	fakeProcTree{
		name: map[int]string{
			callerPID: "tclaude", harnessPID: copilotSEAComm,
			bwrapPID: "bwrap", panePID: "sh",
		},
		exe: map[int]string{harnessPID: harness.CopilotName},
		parent: map[int]int{
			callerPID: harnessPID, harnessPID: bwrapPID,
			bwrapPID: panePID, panePID: 1,
		},
	}.install(t)

	resolved, _ := hookSessionRowForPID(callerPID)
	require.NotNil(t, resolved)
	require.Equal(t, oldLabel, resolved.ID,
		"fixture must reproduce the startup callback resolving to the reused-pid row")

	previousPaneProbe := brokerLivePaneProbe
	probeCalls := 0
	brokerLivePaneProbe = func(tmux string) (lifecyclePaneProbe, error) {
		if tmux == "tmux-new" {
			probeCalls++
			if probeCalls == 1 {
				return lifecyclePaneProbe{state: paneProbeUnknown}, nil
			}
			return lifecyclePaneProbe{
				state: paneProbeLive, panePID: panePID, generation: generation,
			}, nil
		}
		return lifecyclePaneProbe{state: paneProbeUnknown}, nil
	}
	t.Cleanup(func() { brokerLivePaneProbe = previousPaneProbe })

	got, gotHarnessPID := claimedLivePaneSessionRow(callerPID, newLabel)
	require.NotNil(t, got)
	assert.GreaterOrEqual(t, probeCalls, 2,
		"the startup proof must retry while tmux has not published the pane identity")
	assert.Equal(t, newLabel, got.ID)
	assert.Equal(t, harnessPID, gotHarnessPID,
		"the repaired hook keeps the same harness-pid correction as ordinary resolution")

	// Pin both production call sites, not merely the resolver helper. An ack
	// avoids unrelated hook side effects while still proving which row the
	// endpoint used: the token is bound to the new row and would conflict if
	// the stale ancestry result survived the claim check.
	token, err := registerHookAck(newLabel, nil, nil)
	require.NoError(t, err)
	hookBody, err := json.Marshal(session.BrokeredHookRequest{
		ClaimedSessionID: newLabel,
		AckToken:         token,
	})
	require.NoError(t, err)
	hookReq := httptest.NewRequest(http.MethodPost, "/v1/whoami/hook", bytes.NewReader(hookBody))
	hookReq = hookReq.WithContext(context.WithValue(hookReq.Context(), peerKey{}, &peer{
		PID: callerPID, ConvID: "old-conv", HasClaudeAncestor: true,
	}))
	hookRec := httptest.NewRecorder()
	handleWhoamiHook(hookRec, hookReq)
	assert.Equal(t, http.StatusOK, hookRec.Code, "hook response: %s", hookRec.Body.String())

	renderBody, err := json.Marshal(statusbar.BrokeredRenderRequest{
		ClaimedSessionID: newLabel,
		RenderConvID:     "new-conv",
	})
	require.NoError(t, err)
	renderReq := httptest.NewRequest(http.MethodPost, "/v1/whoami/statusline", bytes.NewReader(renderBody))
	renderReq = renderReq.WithContext(context.WithValue(renderReq.Context(), peerKey{}, &peer{
		PID: callerPID, ConvID: "old-conv", HasClaudeAncestor: true,
	}))
	renderRec := httptest.NewRecorder()
	handleWhoamiStatusline(renderRec, renderReq)
	assert.Equal(t, http.StatusOK, renderRec.Code, "statusline response: %s", renderRec.Body.String())

	// A reused pid is what turns this launch gap into a mismatch. Without that
	// accident ordinary resolution finds no row, and both endpoints must still
	// reach the same generation-bound live-pane proof.
	require.NoError(t, db.DeleteSession(oldLabel))
	none, _ := hookSessionRowForPID(callerPID)
	require.Nil(t, none, "without the reused-pid row ordinary startup resolution has no candidate")

	token, err = registerHookAck(newLabel, nil, nil)
	require.NoError(t, err)
	hookBody, err = json.Marshal(session.BrokeredHookRequest{
		ClaimedSessionID: newLabel,
		AckToken:         token,
	})
	require.NoError(t, err)
	hookReq = httptest.NewRequest(http.MethodPost, "/v1/whoami/hook", bytes.NewReader(hookBody))
	hookReq = hookReq.WithContext(context.WithValue(hookReq.Context(), peerKey{}, &peer{
		PID: callerPID, HasClaudeAncestor: true,
	}))
	hookRec = httptest.NewRecorder()
	handleWhoamiHook(hookRec, hookReq)
	assert.Equal(t, http.StatusOK, hookRec.Code, "nil-resolution hook response: %s", hookRec.Body.String())

	renderReq = httptest.NewRequest(http.MethodPost, "/v1/whoami/statusline", bytes.NewReader(renderBody))
	renderReq = renderReq.WithContext(context.WithValue(renderReq.Context(), peerKey{}, &peer{
		PID: callerPID, HasClaudeAncestor: true,
	}))
	renderRec = httptest.NewRecorder()
	handleWhoamiStatusline(renderRec, renderReq)
	assert.Equal(t, http.StatusOK, renderRec.Code,
		"nil-resolution statusline response: %s", renderRec.Body.String())

	persisted, err := db.LoadSession(newLabel)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	persisted.PID = panePID
	require.NoError(t, db.SaveSession(persisted))
	afterPID, _ := claimedLivePaneSessionRow(callerPID, newLabel)
	require.NotNil(t, afterPID,
		"the generation-bound pane proof must remain available after the launch parent records its pid")
	assert.Equal(t, newLabel, afterPID.ID)

	brokerLivePaneProbe = func(string) (lifecyclePaneProbe, error) {
		return lifecyclePaneProbe{
			state: paneProbeLive, panePID: panePID, generation: "later-reused-pane",
		}, nil
	}
	stale, _ := claimedLivePaneSessionRow(callerPID, newLabel)
	assert.Nil(t, stale,
		"a later pane reusing the tmux name must not prove a stale launch row")
}

// The launch parent eventually replaces pid=0 with the pane pid, but that does
// not necessarily heal the collision. A stale row keyed by the newly reused
// HARNESS pid wins at the first lookup in hookSessionRowForPID, before the walk
// can reach the new row's pane pid. This is the sustained failure visible as a
// stream of rejected SessionStart/tool/Stop hooks until stop+resume changes the
// harness pid.
func TestClaimedLivePaneSessionRowRepairsSustainedHarnessPIDCollision(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(ResetBrokerLimiterForTest())

	const (
		callerPID  = 6101
		harnessPID = 6090
		bwrapPID   = 6080
		panePID    = 6070
		oldLabel   = "spwn-old-harness-pid"
		newLabel   = "spwn-new-running"
		generation = "launch-new-running"
	)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: oldLabel, PID: harnessPID, ConvID: "old-conv", TmuxSession: "tmux-old",
		Harness: harness.CopilotName, Status: "working",
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
	}))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: newLabel, PID: panePID, ConvID: "new-conv", TmuxSession: "tmux-new",
		Harness: harness.CopilotName, Status: "idle",
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		ExitLaunchGeneration:  generation,
	}))
	fakeProcTree{
		name: map[int]string{
			callerPID: "tclaude", harnessPID: copilotSEAComm,
			bwrapPID: "bwrap", panePID: "sh",
		},
		exe: map[int]string{harnessPID: harness.CopilotName},
		parent: map[int]int{
			callerPID: harnessPID, harnessPID: bwrapPID,
			bwrapPID: panePID, panePID: 1,
		},
	}.install(t)

	resolved, _ := hookSessionRowForPID(callerPID)
	require.NotNil(t, resolved)
	require.Equal(t, oldLabel, resolved.ID,
		"the stale harness-pid row must keep winning after the new pane pid is persisted")

	previousPaneProbe := brokerLivePaneProbe
	brokerLivePaneProbe = func(tmux string) (lifecyclePaneProbe, error) {
		if tmux == "tmux-new" {
			return lifecyclePaneProbe{
				state: paneProbeLive, panePID: panePID, generation: generation,
			}, nil
		}
		return lifecyclePaneProbe{state: paneProbeUnknown}, nil
	}
	t.Cleanup(func() { brokerLivePaneProbe = previousPaneProbe })

	got, gotHarnessPID := claimedLivePaneSessionRow(callerPID, newLabel)
	require.NotNil(t, got)
	assert.Equal(t, newLabel, got.ID)
	assert.Equal(t, harnessPID, gotHarnessPID)

	token, err := registerHookAck(newLabel, nil, nil)
	require.NoError(t, err)
	hookBody, err := json.Marshal(session.BrokeredHookRequest{
		ClaimedSessionID: newLabel,
		AckToken:         token,
	})
	require.NoError(t, err)
	hookReq := httptest.NewRequest(http.MethodPost, "/v1/whoami/hook", bytes.NewReader(hookBody))
	hookReq = hookReq.WithContext(context.WithValue(hookReq.Context(), peerKey{}, &peer{
		PID: callerPID, ConvID: "old-conv", HasClaudeAncestor: true,
	}))
	hookRec := httptest.NewRecorder()
	handleWhoamiHook(hookRec, hookReq)
	assert.Equal(t, http.StatusOK, hookRec.Code, "sustained-collision hook response: %s", hookRec.Body.String())

	renderBody, err := json.Marshal(statusbar.BrokeredRenderRequest{
		ClaimedSessionID: newLabel,
		RenderConvID:     "new-conv",
	})
	require.NoError(t, err)
	renderReq := httptest.NewRequest(http.MethodPost, "/v1/whoami/statusline", bytes.NewReader(renderBody))
	renderReq = renderReq.WithContext(context.WithValue(renderReq.Context(), peerKey{}, &peer{
		PID: callerPID, ConvID: "old-conv", HasClaudeAncestor: true,
	}))
	renderRec := httptest.NewRecorder()
	handleWhoamiStatusline(renderRec, renderReq)
	assert.Equal(t, http.StatusOK, renderRec.Code,
		"sustained-collision statusline response: %s", renderRec.Body.String())

	defaultBrokerLimiter.mu.Lock()
	defer defaultBrokerLimiter.mu.Unlock()
	assert.NotContains(t, defaultBrokerLimiter.buckets, oldLabel,
		"a proved request must never be charged to the provisional stale row")
	provedBucket := defaultBrokerLimiter.buckets[newLabel]
	require.NotNil(t, provedBucket)
	assert.Equal(t, 2, provedBucket.count,
		"hook and statusline must each charge the proved session exactly once")
	assert.NotContains(t, defaultBrokerLimiter.buckets, brokerPreIdentityKey,
		"a placed caller must not consume the global unplaceable bucket")
	preProofBucket := defaultBrokerLimiter.buckets[brokerPreIdentityKeyForRow(oldLabel)]
	require.NotNil(t, preProofBucket)
	assert.Equal(t, 2, preProofBucket.count,
		"the namespaced provisional-row bucket must bound both requests before identity is proved")
	proofBucket := defaultBrokerLimiter.buckets[brokerProofKeyForRow(oldLabel)]
	require.NotNil(t, proofBucket)
	assert.Equal(t, 2, proofBucket.count,
		"the hard proof bucket must count only the two exceptional tmux proofs")
}

func TestClaimedLivePaneSessionRowRequiresTheClaimedPanesAncestry(t *testing.T) {
	setupTestDB(t)

	const (
		callerPID   = 8101
		copilotPID  = 8090
		ownPanePID  = 8080
		peerPanePID = 7070
		peerLabel   = "spwn-peer"
	)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: peerLabel, PID: 0, ConvID: "peer-conv", TmuxSession: "tmux-peer",
		Harness:               harness.CopilotName,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		ExitLaunchGeneration:  "peer-generation",
	}))
	fakeProcTree{
		name:   map[int]string{callerPID: "tclaude", copilotPID: "copilot", ownPanePID: "sh"},
		parent: map[int]int{callerPID: copilotPID, copilotPID: ownPanePID, ownPanePID: 1},
	}.install(t)

	previousPaneProbe := brokerLivePaneProbe
	brokerLivePaneProbe = func(string) (lifecyclePaneProbe, error) {
		return lifecyclePaneProbe{
			state: paneProbeLive, panePID: peerPanePID, generation: "peer-generation",
		}, nil
	}
	t.Cleanup(func() { brokerLivePaneProbe = previousPaneProbe })

	row, harnessPID := claimedLivePaneSessionRow(callerPID, peerLabel)
	assert.Nil(t, row, "a live peer pane named by the request is not the caller's identity")
	assert.Zero(t, harnessPID)
}
