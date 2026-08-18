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

	const (
		callerPID  = 9101
		harnessPID = 9090
		bwrapPID   = 9080
		panePID    = 9070
		oldLabel   = "spwn-old-pid-owner"
		newLabel   = "spwn-new-starting"
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

	previousPanePID := brokerLivePanePID
	brokerLivePanePID = func(tmux string) int {
		if tmux == "tmux-new" {
			return panePID
		}
		return 0
	}
	t.Cleanup(func() { brokerLivePanePID = previousPanePID })

	got, gotHarnessPID := claimedLivePaneSessionRow(callerPID, newLabel)
	require.NotNil(t, got)
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

	persisted, err := db.LoadSession(newLabel)
	require.NoError(t, err)
	require.NotNil(t, persisted)
	persisted.PID = panePID
	require.NoError(t, db.SaveSession(persisted))
	closed, _ := claimedLivePaneSessionRow(callerPID, newLabel)
	assert.Nil(t, closed,
		"once the launch row has a pid, a request claim must no longer select it through the startup fallback")
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
	}))
	fakeProcTree{
		name:   map[int]string{callerPID: "tclaude", copilotPID: "copilot", ownPanePID: "sh"},
		parent: map[int]int{callerPID: copilotPID, copilotPID: ownPanePID, ownPanePID: 1},
	}.install(t)

	previousPanePID := brokerLivePanePID
	brokerLivePanePID = func(string) int { return peerPanePID }
	t.Cleanup(func() { brokerLivePanePID = previousPanePID })

	row, harnessPID := claimedLivePaneSessionRow(callerPID, peerLabel)
	assert.Nil(t, row, "a live peer pane named by the request is not the caller's identity")
	assert.Zero(t, harnessPID)
}
