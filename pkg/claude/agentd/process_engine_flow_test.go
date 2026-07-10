package agentd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	processengine "github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
	"github.com/tofutools/tclaude/pkg/claude/process/worklist"
	"github.com/tofutools/tclaude/pkg/claude/processcmd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestProcessEngineRoutes404WhenFeatureOff(t *testing.T) {
	f := newFlow(t)
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/process/runs", nil)))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/process/worklist", nil)))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/process/worklist/wi_missing/action", map[string]string{
		"action": "approve", "comment": "reviewed", "idempotencyKey": "off-1",
	})))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestProcessEngineDynamicallyFollowsFeatureFlag(t *testing.T) {
	f := newFlow(t)
	root := filepath.Join(f.World.HomeDir, ".tclaude", "processes")
	t.Cleanup(agentd.SetProcessStoreRootForTest(root))
	require.NoError(t, os.MkdirAll(filepath.Dir(config.ConfigPath()), 0o755))
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":false}}`), 0o644))
	firstOutput := filepath.Join(t.TempDir(), "enabled-output")
	createEngineRun(t, root, "dynamic-enabled", programTemplate("dynamic-enabled", model.Performer{
		Kind: model.PerformerProgram, Run: "/bin/sh", Args: []string{"-c", `touch "$1"`, "process-test", firstOutput},
	}), true)
	stop := make(chan struct{})
	done := agentd.StartProcessEngineForTest(stop, 5*time.Millisecond)
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			close(stop)
			<-done
		}
	})

	time.Sleep(30 * time.Millisecond)
	_, err := os.Stat(firstOutput)
	assert.ErrorIs(t, err, os.ErrNotExist, "disabled engine must not pick up runs")
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":true}}`), 0o644))
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(firstOutput)
		return statErr == nil
	}, 2*time.Second, 10*time.Millisecond)

	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":false}}`), 0o644))
	time.Sleep(30 * time.Millisecond)
	secondOutput := filepath.Join(t.TempDir(), "disabled-output")
	createEngineRun(t, root, "dynamic-disabled", programTemplate("dynamic-disabled", model.Performer{
		Kind: model.PerformerProgram, Run: "/bin/sh", Args: []string{"-c", `touch "$1"`, "process-test", secondOutput},
	}), true)
	time.Sleep(50 * time.Millisecond)
	_, err = os.Stat(secondOutput)
	assert.ErrorIs(t, err, os.ErrNotExist, "turning the flag off must stop new work")
	close(stop)
	<-done
}

func TestProcessEngineDrivesProgramRunEndToEnd(t *testing.T) {
	f, root := processEngineFlow(t)
	output := filepath.Join(t.TempDir(), "program-count.txt")
	fs := createEngineRun(t, root, "program-run", programTemplate("program", model.Performer{
		Kind: model.PerformerProgram,
		Run:  "/bin/sh",
		Args: []string{"-c", `printf 'ran\n' >> "$1"`, "process-test", output},
	}), true)
	host := processengine.New(fs, "agentd:e2e", map[model.PerformerKind]processexec.Adapter{
		model.PerformerProgram: processexec.ProgramAdapter{},
	})

	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Error)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	data, err := os.ReadFile(output)
	require.NoError(t, err)
	assert.Equal(t, "ran\n", string(data))

	rec := processEngineGet(t, f, "/v1/process/runs/program-run")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view struct {
		State struct {
			Status state.RunStatus `json:"status"`
		} `json:"state"`
	}
	testharness.DecodeJSON(t, rec, &view)
	assert.Equal(t, state.RunStatusCompleted, view.State.Status)

	counting := &countingProcessStore{Store: fs}
	idleHost := processengine.New(counting, "agentd:terminal-scan", nil)
	results, err = agentd.RunProcessEngineTickForTest(t.Context(), idleHost)
	require.NoError(t, err)
	require.Len(t, results, 1)
	loads, leases := counting.counts()
	assert.Zero(t, loads, "terminal checkpoint must not load evidence")
	assert.Zero(t, leases, "terminal checkpoint must not churn leases")
}

func TestProcessEngineDrivesCodeChangeStrawmanHappyPath(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, marker := createCapstoneRun(t, root, "capstone-happy", 1)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)

	capstoneTickWaiting(t, host, "implement.plan")
	capstoneReportAgent(t, f, fs, "capstone-happy", "implement.plan", "artifact:plan")
	capstoneTickWaiting(t, host, "implement.plan.approval")
	capstoneReplyHuman(t, fs, "capstone-happy", "implement.plan.approval", "approve plan reviewed")
	capstoneTickWaiting(t, host, "implement.do")
	capstoneReportAgent(t, f, fs, "capstone-happy", "implement.do", "commit:happy")

	// The real hermetic program check executes inside this tick, after which
	// the engine advances unattended to the agent cold-review obligation.
	capstoneTickWaiting(t, host, "implement.test.cold-review")
	capstoneReportAgent(t, f, fs, "capstone-happy", "implement.test.cold-review", "review:happy")
	capstoneTickWaiting(t, host, "implement.review")
	capstoneReplyHuman(t, fs, "capstone-happy", "implement.review", "approve merge")
	result := capstoneTick(t, host)
	assert.Equal(t, state.RunStatusCompleted, result.Status)

	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "1\n", string(data))
	assertCapstoneAuditableFromRunDir(t, root, "capstone-happy")
}

func TestProcessEngineCodeChangePoisonDecisionRetrySurvivesRestart(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, marker := createCapstoneRun(t, root, "capstone-retry", 3)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)

	capstoneReachDo(t, f, fs, host, "capstone-retry")
	capstoneReportAgent(t, f, fs, "capstone-retry", "implement.do", "commit:retry-1")
	capstoneTickWaiting(t, host, "implement.do") // first check failure feeds back
	capstoneReportAgent(t, f, fs, "capstone-retry", "implement.do", "commit:retry-2")
	capstoneTickWaiting(t, host, "escalate") // second failure poisons, then offers the decision

	blocked, err := fs.LoadRun(t.Context(), "capstone-retry")
	require.NoError(t, err)
	assert.Equal(t, state.NodeStatusBlocked, blocked.State.Nodes["implement"].Status)
	assert.Equal(t, state.NodeStatusBlocked, blocked.State.Nodes["implement.test.tests"].Status)
	assert.Contains(t, blocked.State.Nodes["implement"].BlockedReason, "exhausted its budget of 2 failed verdicts")
	capstoneReplyHuman(t, fs, "capstone-retry", "escalate", "retry transient failure reviewed")

	// Simulate daemon death immediately after the planner/executor has durably
	// claimed resolve_block, before it can append the audited resolution.
	ctx, cancel := context.WithCancel(t.Context())
	crashStore := &cancelAfterResolveClaimStore{Store: fs, cancel: cancel}
	beforeRestart := processengine.New(crashStore, "agentd:capstone-before-restart", nil)
	results, err := agentd.RunProcessEngineTickForTest(ctx, beforeRestart)
	if err != nil {
		assert.ErrorIs(t, err, context.Canceled)
	} else {
		require.Len(t, results, 1)
		assert.Contains(t, results[0].Error, context.Canceled.Error())
	}
	claimed, err := fs.LoadRun(t.Context(), "capstone-retry")
	require.NoError(t, err)
	resolveID := outstandingCommandForNode(t, claimed.State, "escalate", state.CommandKindResolveBlock)
	assert.Equal(t, state.CommandStatusIssued, claimed.State.OutstandingCommands[resolveID].Status)
	assert.Equal(t, state.NodeStatusBlocked, claimed.State.Nodes["implement"].Status)

	// A fresh production host rediscovers the claimed command, applies the
	// existing ResolveBlocked funnel exactly once, and continues into attempt 3.
	restarted, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	capstoneTickWaiting(t, restarted, "implement.test.cold-review")
	resolved, err := fs.LoadRun(t.Context(), "capstone-retry")
	require.NoError(t, err)
	assert.Equal(t, state.CommandStatusObserved, resolved.State.OutstandingCommands[resolveID].Status)
	assert.Equal(t, 1, blockResolutionCount(resolved.State))
	assert.Equal(t, state.BlockDecisionRetry, resolved.State.Nodes["implement"].BlockResolution.Decision)

	capstoneReportAgent(t, f, fs, "capstone-retry", "implement.test.cold-review", "review:retry")
	capstoneTickWaiting(t, restarted, "implement.review")
	capstoneReplyHuman(t, fs, "capstone-retry", "implement.review", "approve merge")
	result := capstoneTick(t, restarted)
	assert.Equal(t, state.RunStatusCompleted, result.Status)
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "3\n", string(data), "restart must not execute a program idempotency key twice")
	assertCapstoneAuditableFromRunDir(t, root, "capstone-retry")
}

func TestProcessEngineCodeChangePoisonDecisionCancel(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, marker := createCapstoneRun(t, root, "capstone-cancel", 99)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)

	capstoneReachDo(t, f, fs, host, "capstone-cancel")
	capstoneReportAgent(t, f, fs, "capstone-cancel", "implement.do", "commit:cancel-1")
	capstoneTickWaiting(t, host, "implement.do")
	capstoneReportAgent(t, f, fs, "capstone-cancel", "implement.do", "commit:cancel-2")
	capstoneTickWaiting(t, host, "escalate")
	capstoneReplyHuman(t, fs, "capstone-cancel", "escalate", "cancel do not merge")

	ctx, cancel := context.WithCancel(t.Context())
	crashStore := &cancelAfterResolveClaimStore{Store: fs, cancel: cancel}
	beforeRestart := processengine.New(crashStore, "agentd:cancel-before-restart", nil)
	results, err := agentd.RunProcessEngineTickForTest(ctx, beforeRestart)
	if err != nil {
		assert.ErrorIs(t, err, context.Canceled)
	} else {
		require.Len(t, results, 1)
		assert.Contains(t, results[0].Error, context.Canceled.Error())
	}
	claimed, err := fs.LoadRun(t.Context(), "capstone-cancel")
	require.NoError(t, err)
	resolveID := outstandingCommandForNode(t, claimed.State, "escalate", state.CommandKindResolveBlock)
	assert.Equal(t, state.NodeStatusBlocked, claimed.State.Nodes["implement"].Status)
	assert.Equal(t, state.NodeStatusPending, claimed.State.Nodes["canceled"].Status)

	restarted, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	result := capstoneTick(t, restarted)
	assert.Equal(t, state.RunStatusCanceled, result.Status)

	snapshot, err := fs.LoadRun(t.Context(), "capstone-cancel")
	require.NoError(t, err)
	require.NotNil(t, snapshot.State.Nodes["implement"].BlockResolution)
	assert.Equal(t, state.BlockDecisionCancel, snapshot.State.Nodes["implement"].BlockResolution.Decision)
	assert.Equal(t, 1, blockResolutionCount(snapshot.State))
	assert.Equal(t, state.CommandStatusObserved, snapshot.State.OutstandingCommands[resolveID].Status)
	var cancelCommand state.OutstandingCommand
	for _, command := range snapshot.State.OutstandingCommands {
		if command.Kind == state.CommandKindResolveBlock {
			cancelCommand = command
		}
	}
	assert.Equal(t, state.CommandStatusObserved, cancelCommand.Status, "cancel must atomically close its resolve command before the run becomes terminal")
	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	assert.Equal(t, "2\n", string(data))
	assertCapstoneAuditableFromRunDir(t, root, "capstone-cancel")
}

func TestProcessEngineClosesClaimedResolutionSupersededByManualCancel(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, _ := createCapstoneRun(t, root, "capstone-superseded", 99)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)

	capstoneReachDo(t, f, fs, host, "capstone-superseded")
	capstoneReportAgent(t, f, fs, "capstone-superseded", "implement.do", "commit:superseded-1")
	capstoneTickWaiting(t, host, "implement.do")
	capstoneReportAgent(t, f, fs, "capstone-superseded", "implement.do", "commit:superseded-2")
	capstoneTickWaiting(t, host, "escalate")
	capstoneReplyHuman(t, fs, "capstone-superseded", "escalate", "retry reviewed")

	ctx, cancel := context.WithCancel(t.Context())
	crashStore := &cancelAfterResolveClaimStore{Store: fs, cancel: cancel}
	beforeRestart := processengine.New(crashStore, "agentd:superseded-before-restart", nil)
	_, _ = agentd.RunProcessEngineTickForTest(ctx, beforeRestart)
	claimed, err := fs.LoadRun(t.Context(), "capstone-superseded")
	require.NoError(t, err)
	resolveID := outstandingCommandForNode(t, claimed.State, "escalate", state.CommandKindResolveBlock)
	assert.Equal(t, state.CommandStatusIssued, claimed.State.OutstandingCommands[resolveID].Status)

	executor := processexec.New(fs, nil)
	_, err = executor.ResolveBlocked(t.Context(), processexec.BlockResolutionRequest{
		RunID: "capstone-superseded", NodeID: "implement.test.tests", BlockedAttempt: 2,
		Decision: state.BlockDecisionCancel, Actor: "human:operator", Reason: "operator canceled during restart",
		EvidenceRef: "human-message:manual-cancel",
	})
	require.NoError(t, err)

	restarted, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	result := capstoneTick(t, restarted)
	assert.Equal(t, state.RunStatusCanceled, result.Status)
	assert.Empty(t, result.Error)
	closed, err := fs.LoadRun(t.Context(), "capstone-superseded")
	require.NoError(t, err)
	assert.Equal(t, state.CommandStatusObserved, closed.State.OutstandingCommands[resolveID].Status)
	assert.Equal(t, "superseded", closed.State.OutstandingCommands[resolveID].Verdict)
}

func TestProcessEngineConsumedDecisionDoesNotRetryLaterPoison(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, _ := createCapstoneRun(t, root, "capstone-repoison", 99)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)

	capstoneReachDo(t, f, fs, host, "capstone-repoison")
	capstoneReportAgent(t, f, fs, "capstone-repoison", "implement.do", "commit:repoison-1")
	capstoneTickWaiting(t, host, "implement.do")
	capstoneReportAgent(t, f, fs, "capstone-repoison", "implement.do", "commit:repoison-2")
	capstoneTickWaiting(t, host, "escalate")
	capstoneReplyHuman(t, fs, "capstone-repoison", "escalate", "retry reviewed once")

	// The released check fails once within its fresh budget, feeds back into
	// the last allowed do attempt, then fails again into a later poison.
	capstoneTickWaiting(t, host, "implement.do")
	capstoneReportAgent(t, f, fs, "capstone-repoison", "implement.do", "commit:repoison-3")
	result := capstoneTick(t, host)
	assert.Equal(t, state.RunStatusRunning, result.Status)
	snapshot, err := fs.LoadRun(t.Context(), "capstone-repoison")
	require.NoError(t, err)
	assert.Equal(t, state.NodeStatusBlocked, snapshot.State.Nodes["implement"].Status)
	assert.Equal(t, state.NodeStatusCompleted, snapshot.State.Nodes["escalate"].Status)
	assert.Equal(t, 1, blockResolutionCount(snapshot.State), "old human decision must not resolve a later poison")
	for _, command := range snapshot.State.OutstandingCommands {
		if command.Kind == state.CommandKindResolveBlock && command.Status == state.CommandStatusIssued {
			t.Fatalf("old decision emitted a fresh resolution: %#v", command)
		}
	}
}

func TestProcessEngineAgentSpawnReportSettleAndResumeSuppression(t *testing.T) {
	f, root := processEngineFlow(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "process-dev", Harness: "claude"})
	require.NoError(t, err)
	fs := createEngineRun(t, root, "agent-run", programTemplate("agent-process", model.Performer{
		Kind: model.PerformerAgent, Profile: "process-dev", Prompt: "Implement the requested change",
		Contact: &model.ContactSchedule{Cadence: "1h", Budget: 2, EscalationTarget: "human:operator"},
	}), false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Error)

	snapshot, err := fs.LoadRun(t.Context(), "agent-run")
	require.NoError(t, err)
	require.Len(t, snapshot.State.Obligations, 1)
	require.Len(t, snapshot.State.Contacts, 1)
	var commandID string
	for id, command := range snapshot.State.OutstandingCommands {
		if command.Kind == state.CommandKindStartAttempt {
			commandID = id
			assert.NotEmpty(t, command.ExternalRef)
		}
	}
	require.NotEmpty(t, commandID)
	agentRow, err := db.AgentForProcessCommand(commandID)
	require.NoError(t, err)
	require.NotNil(t, agentRow)
	firstAgentID := agentRow.AgentID
	assert.Equal(t, firstAgentID, snapshot.State.OutstandingCommands[commandID].ExternalRef)
	assert.Equal(t, "agent:"+firstAgentID, firstObligation(snapshot.State).Assignee)
	assert.Equal(t, "agent:"+firstAgentID, snapshot.State.Contacts[commandID].Assignee)
	assert.NotEqual(t, agentRow.CurrentConvID, snapshot.State.OutstandingCommands[commandID].ExternalRef)

	// A fresh host rediscovers the metadata-bound actor and leaves it in
	// flight; it must not dispatch a second agent.
	restarted, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	results, err = agentd.RunProcessEngineTickForTest(t.Context(), restarted)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, results[0].Waiting, "waiting on agent:")
	agentRow, err = db.AgentForProcessCommand(commandID)
	require.NoError(t, err)
	require.NotNil(t, agentRow)
	assert.Equal(t, firstAgentID, agentRow.AgentID)

	reportBody := map[string]string{
		"command_id": commandID, "verdict": "pass", "evidence_ref": "commit:abc123",
	}
	foreignConv := "ffff-1111-2222-3333-444444444444"
	_, _, err = db.EnsureAgentForConv(foreignConv, "test")
	require.NoError(t, err)
	denied := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/process/runs/agent-run/nodes/work/report", reportBody), foreignConv))
	require.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
	invalidBody := map[string]string{
		"command_id": commandID, "verdict": "approve", "evidence_ref": "commit:invalid",
	}
	invalid := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/process/runs/agent-run/nodes/work/report", invalidBody), agentRow.CurrentConvID))
	require.Equal(t, http.StatusConflict, invalid.Code, invalid.Body.String())
	assert.Contains(t, invalid.Body.String(), "allowed: pass, fail, ask-changes")

	req := testharness.JSONRequest(t, http.MethodPost, "/v1/process/runs/agent-run/nodes/work/report", reportBody)
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(req, agentRow.CurrentConvID))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	results, err = agentd.RunProcessEngineTickForTest(t.Context(), restarted)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	settled, err := fs.LoadRun(t.Context(), "agent-run")
	require.NoError(t, err)
	assert.Equal(t, state.WaitStatusSatisfied, firstObligation(settled.State).Status)
	assert.Equal(t, state.ActorRef("agent:"+firstAgentID), settled.State.Nodes["work"].ActiveAttempt.Actor)
}

func TestProcessEngineOwnInboxDeliveryDoesNotPreemptAgent(t *testing.T) {
	_, root := processEngineFlow(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "process-preempt", Harness: "claude"})
	require.NoError(t, err)
	fs := createEngineRun(t, root, "agent-delivery-run", programTemplate("agent-delivery", model.Performer{
		Kind: model.PerformerAgent, Profile: "process-preempt", Prompt: "Implement the requested change",
		Contact: &model.ContactSchedule{Cadence: "1s", Budget: 2, EscalationTarget: "human:operator"},
	}), false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)

	snapshot, err := fs.LoadRun(t.Context(), "agent-delivery-run")
	require.NoError(t, err)
	commandID := firstContact(snapshot.State).CommandID
	agentRow, err := db.AgentForProcessCommand(commandID)
	require.NoError(t, err)
	require.NotNil(t, agentRow)
	messageID, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID: 0, ToConv: agentRow.CurrentConvID, Subject: "Process nudge", Body: "continue",
		ToRecipients: []string{agentRow.CurrentConvID},
	})
	require.NoError(t, err)
	require.NoError(t, db.MarkAgentMessageDelivered(messageID))
	message, err := db.GetAgentMessage(messageID)
	require.NoError(t, err)
	require.NotNil(t, message)
	require.False(t, message.DeliveredAt.IsZero())

	sessionRow, err := db.FindSessionByConvID(agentRow.CurrentConvID)
	require.NoError(t, err)
	require.NotNil(t, sessionRow)
	sessionRow.StatusDetail = "UserPromptSubmit"
	sessionRow.LastHook = message.DeliveredAt
	require.NoError(t, db.SaveSession(sessionRow))
	host.Now = func() time.Time { return message.DeliveredAt.Add(6 * time.Second) }
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)

	snapshot, err = fs.LoadRun(t.Context(), "agent-delivery-run")
	require.NoError(t, err)
	contact := firstContact(snapshot.State)
	assert.False(t, contact.Paused)
	assert.True(t, contact.HumanInteractedAt.IsZero())
	assert.Equal(t, 1, contact.Used, "the due nudge proceeds after an automated UserPromptSubmit")
}

func TestProcessEngineHumanObligationAppearsAndResolvesThroughCLI(t *testing.T) {
	_, root := processEngineFlow(t)
	fs := createEngineRun(t, root, "human-run", programTemplate("human-process", model.Performer{
		Kind: model.PerformerHuman, Profile: "johan", Ask: "Approve the release?",
		Contact: &model.ContactSchedule{Cadence: "30m", Budget: 5, EscalationTarget: "human:operator"},
	}), false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	snapshot, err := fs.LoadRun(t.Context(), "human-run")
	require.NoError(t, err)
	obligation := firstObligation(snapshot.State)
	assert.Equal(t, state.WaitStatusPending, obligation.Status)
	assert.Equal(t, "human:johan", obligation.Assignee)
	assert.Equal(t, []string{"approve", "reject", "ask-changes"}, obligation.AvailableActions)
	messages, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	assert.Contains(t, messages[0].Body, "Approve the release?")

	cmd := processcmd.Cmd()
	cmd.SetArgs([]string{"resolve", "human-run", "work", "--store-root", root, "--verdict", "approve", "--actor", "human:johan", "--evidence", "approval:dashboard-1"})
	require.NoError(t, cmd.Execute())
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	settled, err := fs.LoadRun(t.Context(), "human-run")
	require.NoError(t, err)
	assert.Equal(t, state.WaitStatusSatisfied, firstObligation(settled.State).Status)
}

func TestProcessEngineHumanObligationResolvesThroughDashboardMessages(t *testing.T) {
	_, root := processEngineFlow(t)
	fs := createEngineRun(t, root, "human-dashboard-run", programTemplate("human-dashboard-process", model.Performer{
		Kind: model.PerformerHuman, Profile: "operator", Ask: "Approve from Messages?",
	}), false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	messages, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	message := messages[0]
	assert.Equal(t, "human-dashboard-run", message.ProcessRunID)
	assert.NotEmpty(t, message.ProcessCommandID)
	rec := postDashReply(t, dashMessageMux(t), map[string]any{"id": message.ID, "body": "approve looks good in dashboard"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	snapshot, err := fs.LoadRun(t.Context(), "human-dashboard-run")
	require.NoError(t, err)
	assert.Equal(t, state.ActorRef("human:operator"), snapshot.State.Nodes["work"].ActiveAttempt.Actor)
	assert.Equal(t, state.WaitStatusSatisfied, firstObligation(snapshot.State).Status)
}

func TestProcessEngineHumanDecisionAdvertisesAndPreservesEdgeVerdict(t *testing.T) {
	_, root := processEngineFlow(t)
	performer := model.Performer{Kind: model.PerformerHuman, Profile: "operator", Ask: "Approve the release?"}
	fs := createEngineRun(t, root, "human-decision-run", decisionTemplate("human-decision", performer), false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	snapshot, err := fs.LoadRun(t.Context(), "human-decision-run")
	require.NoError(t, err)
	assert.Equal(t, []string{"approve", "reject"}, firstObligation(snapshot.State).AvailableActions)
	messages, err := db.ListHumanMessages()
	require.NoError(t, err)
	require.NotEmpty(t, messages)
	rec := postDashReply(t, dashMessageMux(t), map[string]any{"id": messages[0].ID, "body": "approve release reviewed"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	snapshot, err = fs.LoadRun(t.Context(), "human-decision-run")
	require.NoError(t, err)
	assert.Equal(t, "approve", snapshot.State.Nodes["decide"].ChosenEdge)
}

func TestProcessWorklistActionUsesObservationFunnelAndIsIdempotent(t *testing.T) {
	f, root := processEngineFlow(t)
	performer := model.Performer{
		Kind: model.PerformerHuman, Profile: "operator", Ask: "Approve the release?",
		Contact: &model.ContactSchedule{Cadence: "30m", Budget: 5, EscalationTarget: "human:oncall"},
	}
	fs := createEngineRun(t, root, "worklist-decision-run", decisionTemplate("worklist-decision", performer), false)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)

	rec := processEngineGet(t, f, "/v1/process/worklist?assignee=human:operator&kind=decision-needed&status=pending")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listing struct {
		Items []worklist.Item `json:"items"`
	}
	testharness.DecodeJSON(t, rec, &listing)
	require.Len(t, listing.Items, 1)
	item := listing.Items[0]
	require.NotNil(t, item.Nudge)
	assert.Equal(t, 0, item.Nudge.BudgetUsed)
	assert.Equal(t, 5, item.Nudge.BudgetMax)
	assert.Equal(t, "human:oncall", item.Nudge.EscalationTarget)
	assert.False(t, item.Nudge.NextContactAt.IsZero())

	body := map[string]string{"action": "approve", "comment": "release reviewed", "idempotencyKey": "dashboard-submit-1"}
	rec = humanReq(t, f, http.MethodPost, "/v1/process/worklist/"+item.ID+"/action", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	afterFirst, err := fs.LoadRun(t.Context(), "worklist-decision-run")
	require.NoError(t, err)
	firstSeq := afterFirst.State.LastLogSeq
	assert.Equal(t, state.WaitStatusSatisfied, firstObligation(afterFirst.State).Status)

	// The same submission goes through RecordOutstandingObservation again;
	// its existing observed-command check makes it a true no-op.
	rec = humanReq(t, f, http.MethodPost, "/v1/process/worklist/"+item.ID+"/action", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	afterReplay, err := fs.LoadRun(t.Context(), "worklist-decision-run")
	require.NoError(t, err)
	assert.Equal(t, firstSeq, afterReplay.State.LastLogSeq)

	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	settled, err := fs.LoadRun(t.Context(), "worklist-decision-run")
	require.NoError(t, err)
	require.Len(t, settled.State.Nodes["decide"].Decisions, 1)
	decision := settled.State.Nodes["decide"].Decisions[0]
	assert.Equal(t, state.ActorRef("human:operator"), decision.Actor)
	assert.Equal(t, "approve", decision.Verdict)
	assert.Contains(t, decision.EvidenceRef, "worklist-action:sha256:")
}

func TestProcessWorklistBlockedActionUsesUnblockFunnel(t *testing.T) {
	f, root := processEngineFlow(t)
	fs, _ := createCapstoneRun(t, root, "worklist-blocked-run", 99)
	host, err := agentd.NewProcessEngineHostForTest(root)
	require.NoError(t, err)

	capstoneReachDo(t, f, fs, host, "worklist-blocked-run")
	capstoneReportAgent(t, f, fs, "worklist-blocked-run", "implement.do", "commit:block-1")
	capstoneTickWaiting(t, host, "implement.do")
	capstoneReportAgent(t, f, fs, "worklist-blocked-run", "implement.do", "commit:block-2")
	capstoneTickWaiting(t, host, "escalate")

	rec := processEngineGet(t, f, "/v1/process/worklist?run=worklist-blocked-run&kind=blocked&status=pending")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var listing struct {
		Items []worklist.Item `json:"items"`
	}
	testharness.DecodeJSON(t, rec, &listing)
	require.Len(t, listing.Items, 1)
	item := listing.Items[0]
	assert.Equal(t, "implement.test.tests", item.Node)
	assert.Contains(t, item.Summary, "exhausted its budget")
	assert.Equal(t, "human:operator", item.Assignee)

	body := map[string]string{"action": "retry", "comment": "transient failure reviewed", "idempotencyKey": "blocked-submit-1"}
	rec = humanReq(t, f, http.MethodPost, "/v1/process/worklist/"+item.ID+"/action", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resolved, err := fs.LoadRun(t.Context(), "worklist-blocked-run")
	require.NoError(t, err)
	assert.Equal(t, 1, blockResolutionCount(resolved.State))
	require.NotNil(t, resolved.State.Nodes["implement.test.tests"].BlockResolution)
	assert.Equal(t, state.BlockDecisionRetry, resolved.State.Nodes["implement.test.tests"].BlockResolution.Decision)
	assert.Equal(t, state.ActorRef("human:operator"), resolved.State.Nodes["implement.test.tests"].BlockResolution.Actor)

	rec = humanReq(t, f, http.MethodPost, "/v1/process/worklist/"+item.ID+"/action", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	replayed, err := fs.LoadRun(t.Context(), "worklist-blocked-run")
	require.NoError(t, err)
	assert.Equal(t, 1, blockResolutionCount(replayed.State), "idempotent replay appended another resolution audit")
}

func TestProcessEngineNudgeBudgetEscalatesAndResetsOnRecovery(t *testing.T) {
	_, root := processEngineFlow(t)
	adapter := &deferredContactAdapter{}
	fs := createEngineRun(t, root, "nudge-run", programTemplate("nudge", model.Performer{
		Kind: model.PerformerAgent, Profile: "fake", Prompt: "work",
		Contact: &model.ContactSchedule{Cadence: "1s", Budget: 1, EscalationTarget: "human:oncall"},
	}), false)
	now := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	host := processengine.New(fs, "agentd:nudges", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: adapter})
	host.Now = func() time.Time { return now }
	_, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)

	now = now.Add(2 * time.Second)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	assert.Equal(t, 1, adapter.nudges)
	assert.Equal(t, 0, adapter.escalations)

	now = now.Add(2 * time.Second)
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	assert.Equal(t, 1, adapter.nudges)
	assert.Equal(t, 1, adapter.escalations)
	snapshot, err := fs.LoadRun(t.Context(), "nudge-run")
	require.NoError(t, err)
	contact := firstContact(snapshot.State)
	assert.False(t, contact.EscalatedAt.IsZero())
	assert.Equal(t, 1, contact.Used)
	assert.Equal(t, state.RunStatusRunning, snapshot.State.Status, "exhaustion escalates and keeps waiting")

	now = now.Add(time.Second)
	adapter.activity = processexec.Activity{Recovered: true, At: now}
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	snapshot, err = fs.LoadRun(t.Context(), "nudge-run")
	require.NoError(t, err)
	contact = firstContact(snapshot.State)
	assert.Zero(t, contact.Used)
	assert.True(t, contact.EscalatedAt.IsZero())
	assert.Equal(t, now.Add(time.Second), contact.NextContactAt)
}

func TestProcessEngineHumanPreemptionPausesAgentAutomation(t *testing.T) {
	_, root := processEngineFlow(t)
	adapter := &deferredContactAdapter{}
	fs := createEngineRun(t, root, "preempt-run", programTemplate("preempt", model.Performer{
		Kind: model.PerformerAgent, Profile: "fake", Prompt: "work",
		Contact: &model.ContactSchedule{Cadence: "1s", Budget: 2, EscalationTarget: "human:oncall"},
	}), false)
	now := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)
	host := processengine.New(fs, "agentd:preempt", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: adapter})
	host.Now = func() time.Time { return now }
	_, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	now = now.Add(10 * time.Second)
	adapter.activity = processexec.Activity{HumanInteracted: true, At: now.Add(-6 * time.Second)}
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, results[0].Waiting, "automation paused")
	assert.Zero(t, adapter.nudges)
	snapshot, err := fs.LoadRun(t.Context(), "preempt-run")
	require.NoError(t, err)
	assert.True(t, firstContact(snapshot.State).Paused)

	// Real agent activity clears the human-preemption latch and schedules the
	// next contact from a fresh budget.
	now = now.Add(time.Second)
	adapter.activity = processexec.Activity{Recovered: true, At: now}
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	snapshot, err = fs.LoadRun(t.Context(), "preempt-run")
	require.NoError(t, err)
	contact := firstContact(snapshot.State)
	assert.False(t, contact.Paused)
	assert.Empty(t, contact.PauseReason)
	assert.True(t, contact.HumanInteractedAt.IsZero())
}

func TestProcessEngineAutomatedDeliveryDoesNotPauseAgent(t *testing.T) {
	_, root := processEngineFlow(t)
	adapter := &deferredContactAdapter{}
	fs := createEngineRun(t, root, "automated-delivery-run", programTemplate("automated-delivery", model.Performer{
		Kind: model.PerformerAgent, Profile: "fake", Prompt: "work",
		Contact: &model.ContactSchedule{Cadence: "1s", Budget: 2, EscalationTarget: "human:oncall"},
	}), false)
	now := time.Date(2026, 7, 10, 1, 30, 0, 0, time.UTC)
	host := processengine.New(fs, "agentd:automated-delivery", map[model.PerformerKind]processexec.Adapter{model.PerformerAgent: adapter})
	host.Now = func() time.Time { return now }
	_, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)

	now = now.Add(10 * time.Second)
	adapter.activity = processexec.Activity{AutomatedDelivery: true, At: now.Add(-6 * time.Second)}
	_, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	snapshot, err := fs.LoadRun(t.Context(), "automated-delivery-run")
	require.NoError(t, err)
	assert.False(t, firstContact(snapshot.State).Paused)
	assert.Equal(t, 1, adapter.nudges)
}

type deferredContactAdapter struct {
	nudges      int
	escalations int
	activity    processexec.Activity
}

func (*deferredContactAdapter) Validate(processexec.Request) error { return nil }
func (*deferredContactAdapter) Perform(context.Context, processexec.Request) (processexec.Observation, error) {
	return processexec.Observation{}, errors.New("unexpected synchronous perform")
}
func (*deferredContactAdapter) Dispatch(context.Context, processexec.Request) (processexec.DispatchResult, error) {
	return processexec.DispatchResult{ExternalRef: "agent:agt_fake", Assignee: "agent:agt_fake", Summary: "fake work", CreateObligation: true}, nil
}
func (*deferredContactAdapter) ReconcileDeferred(context.Context, processexec.Request) (processexec.Observation, processexec.DeferredStatus, error) {
	return processexec.Observation{}, processexec.DeferredInFlight, nil
}
func (a *deferredContactAdapter) Contact(_ context.Context, _ processexec.Request, escalation bool) error {
	if escalation {
		a.escalations++
	} else {
		a.nudges++
	}
	return nil
}
func (a *deferredContactAdapter) Activity(context.Context, processexec.Request, time.Time) (processexec.Activity, error) {
	return a.activity, nil
}

func firstContact(st *state.State) state.ContactState {
	for _, contact := range st.Contacts {
		return contact
	}
	return state.ContactState{}
}

func firstObligation(st *state.State) state.ObligationRecord {
	for _, obligation := range st.Obligations {
		return obligation
	}
	return state.ObligationRecord{}
}

func TestProcessEngineLeaseContentionAllowsOnlyOneScheduler(t *testing.T) {
	_, root := processEngineFlow(t)
	adapter := newBlockingAdapter()
	fs := createEngineRun(t, root, "lease-run", programTemplate("lease", model.Performer{Kind: model.PerformerProgram, Run: "/fake"}), true)
	first := processengine.New(fs, "agentd:first", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	first.LeaseTTL = 150 * time.Millisecond
	second := processengine.New(fs, "agentd:second", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	second.LeaseTTL = 150 * time.Millisecond

	firstDone := make(chan []processengine.RunResult, 1)
	go func() {
		results, _ := agentd.RunProcessEngineTickForTest(t.Context(), first)
		firstDone <- results
	}()
	select {
	case <-adapter.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first scheduler never reached performer")
	}
	// Wait past the original TTL: contention must still hold because the
	// first host heartbeats at TTL/3 while its performer is running.
	time.Sleep(2 * first.LeaseTTL)
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), second)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.True(t, results[0].LeaseContended, "result: %+v", results[0])
	assert.Contains(t, results[0].Error, store.ErrLeaseHeld.Error())
	close(adapter.release)
	select {
	case results = <-firstDone:
		require.Len(t, results, 1)
		assert.Empty(t, results[0].Error)
	case <-time.After(2 * time.Second):
		t.Fatal("first scheduler did not finish")
	}
}

func TestProcessEngineRestartReconcilesWithoutDoubleExecution(t *testing.T) {
	f, root := processEngineFlow(t)
	adapter := newCrashDiscoverAdapter()
	fs := createEngineRun(t, root, "resume-run", programTemplate("resume", model.Performer{Kind: model.PerformerProgram, Run: "/discoverable"}), true)
	first := processengine.New(fs, "agentd:before-restart", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	first.Executor.ReconcileDelay = -time.Nanosecond
	ctx, cancel := context.WithCancel(t.Context())
	firstDone := make(chan struct{})
	go func() {
		_, _ = agentd.RunProcessEngineTickForTest(ctx, first)
		close(firstDone)
	}()
	select {
	case <-adapter.performed:
		cancel() // crash after the external result exists, before observation
	case <-time.After(2 * time.Second):
		t.Fatal("performer side effect did not start")
	}
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first host did not stop")
	}

	second := processengine.New(fs, "agentd:after-restart", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), second)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Error)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	assert.Equal(t, 1, adapter.performCount(), "restart must reconcile, never perform twice")

	rec := processEngineGet(t, f, "/v1/process/runs/resume-run")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"status":"completed"`)
}

func TestProcessEngineFiresDueTimer(t *testing.T) {
	f, root := processEngineFlow(t)
	fs := createEngineRun(t, root, "timer-run", timerTemplate("timer", time.Minute), false)
	now := time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC)
	host := processengine.New(fs, "agentd:timer", nil)
	host.Now = func() time.Time { return now }

	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusRunning, results[0].Status)
	snapshot, err := fs.LoadRun(t.Context(), "timer-run")
	require.NoError(t, err)
	require.Len(t, snapshot.State.Timers, 1)
	for _, timer := range snapshot.State.Timers {
		assert.Equal(t, now.Add(time.Minute), timer.DueAt)
		assert.Equal(t, state.WaitStatusPending, timer.Status)
	}

	now = now.Add(2 * time.Minute)
	results, err = agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	rec := processEngineGet(t, f, "/v1/process/runs/timer-run")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"status":"satisfied"`)
}

func TestProcessEngineRateLimitPauseSurvivesRestartWithoutRetryBudget(t *testing.T) {
	f, root := processEngineFlow(t)
	start := time.Date(2026, 7, 9, 21, 0, 0, 0, time.UTC)
	adapter := &rateLimitThenPassAdapter{until: start.Add(10 * time.Minute)}
	fs := createEngineRun(t, root, "rate-run", programTemplate("rate", model.Performer{Kind: model.PerformerProgram, Run: "/quota"}), true)
	first := processengine.New(fs, "agentd:rate-before", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	first.Now = func() time.Time { return start }

	results, err := agentd.RunProcessEngineTickForTest(t.Context(), first)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusPaused, results[0].Status)
	assert.Contains(t, results[0].Waiting, "rate limited until")
	snapshot, err := fs.LoadRun(t.Context(), "rate-run")
	require.NoError(t, err)
	require.NotNil(t, snapshot.State.Pause)
	assert.Equal(t, state.PauseKindRateLimited, snapshot.State.Pause.Kind)
	assert.Equal(t, 1, snapshot.State.Nodes["work"].Attempt)

	// A new host reconstructs the pause from state.json, waits through the
	// durable deadline, then retries the same issued command exactly once.
	after := adapter.until.Add(time.Second)
	second := processengine.New(fs, "agentd:rate-after", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	second.Now = func() time.Time { return after }
	results, err = agentd.RunProcessEngineTickForTest(t.Context(), second)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusCompleted, results[0].Status)
	snapshot, err = fs.LoadRun(t.Context(), "rate-run")
	require.NoError(t, err)
	assert.Nil(t, snapshot.State.Pause)
	assert.Equal(t, 1, snapshot.State.Nodes["work"].Attempt, "quota pause must not consume node retry budget")
	assert.Equal(t, 2, adapter.callCount())

	rec := processEngineGet(t, f, "/v1/process/runs/rate-run")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"pause"`)
}

func TestProcessEngineUndiscoverablePerformerParksNeedsReconcile(t *testing.T) {
	f, root := processEngineFlow(t)
	adapter := &errorAdapter{}
	fs := createEngineRun(t, root, "reconcile-run", programTemplate("reconcile", model.Performer{Kind: model.PerformerProgram, Run: "/unknown-result"}), true)
	first := processengine.New(fs, "agentd:claim", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	first.Executor.ReconcileDelay = -time.Nanosecond
	_, err := agentd.RunProcessEngineTickForTest(t.Context(), first)
	require.NoError(t, err)

	second := processengine.New(fs, "agentd:resume", map[model.PerformerKind]processexec.Adapter{model.PerformerProgram: adapter})
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), second)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, state.RunStatusPaused, results[0].Status)
	assert.Contains(t, results[0].Waiting, "needs reconciliation")
	snapshot, err := fs.LoadRun(t.Context(), "reconcile-run")
	require.NoError(t, err)
	require.NotNil(t, snapshot.State.Pause)
	assert.Equal(t, state.PauseKindNeedsReconcile, snapshot.State.Pause.Kind)
	assert.Equal(t, state.ActorRef("human:operator"), snapshot.State.Pause.Owner)
	assert.Equal(t, 1, snapshot.State.Nodes["work"].Attempt)

	rec := processEngineGet(t, f, "/v1/process/runs/reconcile-run")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"kind":"needs_reconcile"`)
	assert.Contains(t, rec.Body.String(), `"owner":"human:operator"`)
}

func TestProcessEngineInconsistentRunHaltsWithVisibleReason(t *testing.T) {
	f, root := processEngineFlow(t)
	output := filepath.Join(t.TempDir(), "must-not-run")
	fs := createEngineRun(t, root, "dirty-run", programTemplate("dirty", model.Performer{
		Kind: model.PerformerProgram,
		Run:  "/bin/sh",
		Args: []string{"-c", `touch "$1"`, "process-test", output},
	}), true)
	manifestPath := filepath.Join(root, "runs", "dirty-run", "manifest.jsonl")
	body, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	lines := bytes.Split(bytes.TrimSpace(body), []byte("\n"))
	require.NotEmpty(t, lines)
	var entry evidence.ManifestEntry
	require.NoError(t, json.Unmarshal(lines[0], &entry))
	entry.EntryChecksum = "deliberately-corrupted"
	lines[0], err = json.Marshal(entry)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, append(bytes.Join(lines, []byte("\n")), '\n'), 0o644))

	host := processengine.New(fs, "agentd:dirty", map[model.PerformerKind]processexec.Adapter{
		model.PerformerProgram: processexec.ProgramAdapter{},
	})
	var firstReason string
	for tick := 0; tick < 2; tick++ {
		results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.Equal(t, state.RunStatusInconsistent, results[0].Status)
		assert.Contains(t, results[0].Waiting, "checksum")
		if tick == 0 {
			firstReason = results[0].Waiting
		} else {
			assert.Equal(t, firstReason, results[0].Waiting, "inconsistent run must stay halted across ticks")
		}
	}
	_, err = os.Stat(output)
	assert.ErrorIs(t, err, os.ErrNotExist)

	rec := processEngineGet(t, f, "/v1/process/runs/dirty-run")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"effectiveStatus":"inconsistent"`)
	assert.Contains(t, rec.Body.String(), "checksum")
}

func processEngineFlow(t *testing.T) (*testharness.Flow, string) {
	t.Helper()
	f := newFlow(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(config.ConfigPath()), 0o755))
	require.NoError(t, os.WriteFile(config.ConfigPath(), []byte(`{"features":{"processes":true}}`), 0o644))
	root := filepath.Join(f.World.HomeDir, ".tclaude", "processes")
	t.Cleanup(agentd.SetProcessStoreRootForTest(root))
	return f, root
}

func processEngineGet(t *testing.T, f *testharness.Flow, path string) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, path, nil)))
}

func createEngineRun(t *testing.T, root, runID string, tmpl *model.Template, allowPrograms bool) *store.FS {
	return createEngineRunWithParams(t, root, runID, tmpl, allowPrograms, nil)
}

func createEngineRunWithParams(t *testing.T, root, runID string, tmpl *model.Template, allowPrograms bool, params map[string]string) *store.FS {
	t.Helper()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	templateRecord, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	nodes := make([]state.NodeInit, 0, len(tmpl.Nodes))
	for id, node := range tmpl.Nodes {
		status := state.NodeStatusPending
		if id == tmpl.Start {
			status = state.NodeStatusReady
		}
		nodes = append(nodes, state.NodeInit{ID: id, Type: node.Type, Status: status})
	}
	initial := state.New(runID, templateRecord.Ref, templateRecord.Ref, nodes)
	initial.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: templateRecord.Ref, Params: params}, initial)
	require.NoError(t, err)
	if allowPrograms {
		at := time.Date(2026, 7, 9, 19, 0, 0, 0, time.UTC)
		_, err = fs.Append(t.Context(), runID, 0, []evidence.LogEntry{{
			SchemaVersion: evidence.LogEntrySchemaVersion,
			At:            at,
			Scope:         evidence.Scope{Kind: evidence.ScopeRun},
			Kind:          evidence.EntryKindAdmin,
			Event: &state.Event{
				Type:   state.EventAdminProgramsAllowed,
				At:     at,
				Actor:  "human:test",
				Reason: "flow test program opt-in",
			},
		}})
		require.NoError(t, err)
		_, err = fs.SetProgramsAllowed(t.Context(), runID)
		require.NoError(t, err)
	}
	return fs
}

func createCapstoneRun(t *testing.T, root, runID string, programPassAt int) (*store.FS, string) {
	t.Helper()
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "dev", Harness: "claude"})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
	require.NoError(t, err)

	source, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "examples", "code-change-with-review.yaml"))
	require.NoError(t, err)
	parsed, err := model.Parse(source)
	require.NoError(t, err)
	require.False(t, parsed.Diagnostics.HasErrors(), "template diagnostics: %#v", parsed.Diagnostics.Errors())
	tmpl := parsed.Template
	implement := tmpl.Nodes["implement"]
	require.Len(t, implement.Checks, 2)
	marker := filepath.Join(t.TempDir(), runID+"-program-count")
	implement.Checks[0].Performer.Run = "/bin/sh"
	implement.Checks[0].Performer.Args = []string{
		"-c",
		`count=0; if [ -f "$1" ]; then read -r count < "$1"; fi; count=$((count + 1)); printf '%s\n' "$count" > "$1"; [ "$count" -ge "$2" ]`,
		"process-capstone", marker, strconv.Itoa(programPassAt),
	}
	tmpl.Nodes["implement"] = implement
	return createEngineRunWithParams(t, root, runID, tmpl, true, map[string]string{"issue": "TCL-278"}), marker
}

func capstoneReachDo(t *testing.T, f *testharness.Flow, fs *store.FS, host *processengine.Host, runID string) {
	t.Helper()
	capstoneTickWaiting(t, host, "implement.plan")
	capstoneReportAgent(t, f, fs, runID, "implement.plan", "artifact:plan")
	capstoneTickWaiting(t, host, "implement.plan.approval")
	capstoneReplyHuman(t, fs, runID, "implement.plan.approval", "approve plan reviewed")
	capstoneTickWaiting(t, host, "implement.do")
}

func capstoneTick(t *testing.T, host *processengine.Host) processengine.RunResult {
	t.Helper()
	results, err := agentd.RunProcessEngineTickForTest(t.Context(), host)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Empty(t, results[0].Error)
	return results[0]
}

func capstoneTickWaiting(t *testing.T, host *processengine.Host, nodeID string) processengine.RunResult {
	t.Helper()
	result := capstoneTick(t, host)
	snapshot, err := host.Store.LoadRun(t.Context(), result.RunID)
	require.NoError(t, err)
	commandID := outstandingCommandForNode(t, snapshot.State, nodeID, "")
	assert.Equal(t, state.CommandStatusIssued, snapshot.State.OutstandingCommands[commandID].Status)
	return result
}

func outstandingCommandForNode(t *testing.T, st *state.State, nodeID string, kind state.CommandKind) string {
	t.Helper()
	for commandID, command := range st.OutstandingCommands {
		if command.NodeID == nodeID && command.Status == state.CommandStatusIssued && (kind == "" || command.Kind == kind) {
			return commandID
		}
	}
	t.Fatalf("no issued %s command for node %s", kind, nodeID)
	return ""
}

func capstoneReportAgent(t *testing.T, f *testharness.Flow, fs *store.FS, runID, nodeID, evidenceRef string) {
	t.Helper()
	snapshot, err := fs.LoadRun(t.Context(), runID)
	require.NoError(t, err)
	commandID := outstandingCommandForNode(t, snapshot.State, nodeID, state.CommandKindStartAttempt)
	agentRow, err := db.AgentForProcessCommand(commandID)
	require.NoError(t, err)
	require.NotNil(t, agentRow)
	brief, ok := f.World.SpawnInitialPrompt(agentRow.CurrentConvID)
	require.True(t, ok)
	assert.Contains(t, brief, "TCL-278")
	assert.NotContains(t, brief, "{{ params.issue }}")
	body := map[string]string{"command_id": commandID, "verdict": "pass", "evidence_ref": evidenceRef}
	req := testharness.JSONRequest(t, http.MethodPost, "/v1/process/runs/"+runID+"/nodes/"+nodeID+"/report", body)
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(req, agentRow.CurrentConvID))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func capstoneReplyHuman(t *testing.T, fs *store.FS, runID, nodeID, reply string) {
	t.Helper()
	snapshot, err := fs.LoadRun(t.Context(), runID)
	require.NoError(t, err)
	commandID := ""
	for id, command := range snapshot.State.OutstandingCommands {
		if command.NodeID == nodeID && command.Status == state.CommandStatusIssued &&
			(command.Kind == state.CommandKindRecordDecision || command.Kind == state.CommandKindStartAttempt) {
			commandID = id
			break
		}
	}
	require.NotEmpty(t, commandID, "no issued human command for %s", nodeID)
	message, err := db.FindHumanMessageForProcessCommand(commandID, "Process obligation")
	require.NoError(t, err)
	require.NotNil(t, message)
	rec := postDashReply(t, dashMessageMux(t), map[string]any{"id": message.ID, "body": reply})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func blockResolutionCount(st *state.State) int {
	count := 0
	for _, record := range st.AdminRecords {
		if record.Type == state.EventBlockResolutionRecorded {
			count++
		}
	}
	return count
}

func assertCapstoneAuditableFromRunDir(t *testing.T, root, runID string) {
	t.Helper()
	runDir := filepath.Join(root, "runs", runID)
	for _, relative := range []string{"run.json", "state.json", "manifest.jsonl", filepath.Join("run", "log.jsonl")} {
		info, err := os.Stat(filepath.Join(runDir, relative))
		require.NoError(t, err, relative)
		assert.Positive(t, info.Size(), relative)
	}
	nodeLogs, err := filepath.Glob(filepath.Join(runDir, "nodes", "*", "log.jsonl"))
	require.NoError(t, err)
	assert.NotEmpty(t, nodeLogs)
	artifacts, err := os.ReadDir(filepath.Join(runDir, "artifacts"))
	require.NoError(t, err)
	assert.NotEmpty(t, artifacts)

	// Remove the store-level template library before reconstruction. A fresh
	// store can still verify from the template snapshot pinned in run.json plus
	// the state/log/manifest/artifact files under this run directory.
	templateArchive := filepath.Join(t.TempDir(), "templates")
	require.NoError(t, os.Rename(filepath.Join(root, "templates"), templateArchive))
	fresh, err := store.NewFS(root)
	require.NoError(t, err)
	report := processverify.StoreRun(t.Context(), fresh, runID)
	assert.False(t, report.HasErrors(), "run-dir verification: %#v", report.Diagnostics)
}

type cancelAfterResolveClaimStore struct {
	store.Store
	cancel context.CancelFunc
	once   sync.Once
}

func (s *cancelAfterResolveClaimStore) Append(ctx context.Context, runID string, expectedSeq int64, entries []evidence.LogEntry) (store.AppendResult, error) {
	result, err := s.Store.Append(ctx, runID, expectedSeq, entries)
	if err != nil {
		return result, err
	}
	for _, entry := range entries {
		if entry.Event != nil && entry.Event.Type == state.EventCommandIssued && entry.Event.Command != nil && entry.Event.Command.Kind == state.CommandKindResolveBlock {
			s.once.Do(s.cancel)
		}
	}
	return result, nil
}

func programTemplate(id string, performer model.Performer) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         id,
		Start:      "work",
		Nodes: map[string]model.Node{
			"work": {Type: model.NodeTypeTask, Performer: &performer, Next: model.Next{"pass": "end"}},
			"end":  {Type: model.NodeTypeEnd},
		},
	}
}

func decisionTemplate(id string, performer model.Performer) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         id,
		Start:      "decide",
		Nodes: map[string]model.Node{
			"decide": {Type: model.NodeTypeDecision, Performer: &performer, Next: model.Next{"approve": "end", "reject": "failed"}},
			"end":    {Type: model.NodeTypeEnd},
			"failed": {Type: model.NodeTypeEnd, Result: "failed"},
		},
	}
}

func timerTemplate(id string, duration time.Duration) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         id,
		Start:      "wait",
		Nodes: map[string]model.Node{
			"wait": {Type: model.NodeTypeWait, Wait: &model.WaitConfig{Duration: duration.String()}, Next: model.Next{"pass": "end"}},
			"end":  {Type: model.NodeTypeEnd},
		},
	}
}

type blockingAdapter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingAdapter() *blockingAdapter {
	return &blockingAdapter{started: make(chan struct{}), release: make(chan struct{})}
}

func (a *blockingAdapter) Validate(processexec.Request) error { return nil }

func (a *blockingAdapter) Perform(ctx context.Context, _ processexec.Request) (processexec.Observation, error) {
	a.once.Do(func() { close(a.started) })
	select {
	case <-ctx.Done():
		return processexec.Observation{}, ctx.Err()
	case <-a.release:
		return processexec.Observation{Actor: "program:fake@exit0", Verdict: "pass"}, nil
	}
}

type crashDiscoverAdapter struct {
	mu           sync.Mutex
	observations map[string]processexec.Observation
	count        int
	performed    chan struct{}
	once         sync.Once
}

func newCrashDiscoverAdapter() *crashDiscoverAdapter {
	return &crashDiscoverAdapter{observations: map[string]processexec.Observation{}, performed: make(chan struct{})}
}

func (a *crashDiscoverAdapter) Validate(processexec.Request) error { return nil }

func (a *crashDiscoverAdapter) Perform(ctx context.Context, request processexec.Request) (processexec.Observation, error) {
	observation := processexec.Observation{Actor: "program:discoverable@exit0", Verdict: "pass", ExternalRef: "external:" + request.Command.IdempotencyKey}
	a.mu.Lock()
	a.count++
	a.observations[request.Command.IdempotencyKey] = observation
	a.mu.Unlock()
	a.once.Do(func() { close(a.performed) })
	<-ctx.Done()
	return processexec.Observation{}, ctx.Err()
}

func (a *crashDiscoverAdapter) Reconcile(_ context.Context, request processexec.Request) (processexec.Observation, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	observation, ok := a.observations[request.Command.IdempotencyKey]
	return observation, ok, nil
}

func (a *crashDiscoverAdapter) performCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.count
}

type rateLimitThenPassAdapter struct {
	mu    sync.Mutex
	calls int
	until time.Time
}

func (a *rateLimitThenPassAdapter) Validate(processexec.Request) error { return nil }

func (a *rateLimitThenPassAdapter) Perform(_ context.Context, _ processexec.Request) (processexec.Observation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.calls == 1 {
		return processexec.Observation{}, &processexec.RateLimitError{Until: a.until}
	}
	return processexec.Observation{Actor: "program:quota@exit0", Verdict: "pass"}, nil
}

func (a *rateLimitThenPassAdapter) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

type errorAdapter struct{}

func (a *errorAdapter) Validate(processexec.Request) error { return nil }

func (a *errorAdapter) Perform(context.Context, processexec.Request) (processexec.Observation, error) {
	return processexec.Observation{}, errors.New("performer result lost")
}

type countingProcessStore struct {
	store.Store
	mu     sync.Mutex
	loads  int
	leases int
}

func (s *countingProcessStore) LoadRun(ctx context.Context, runID string) (store.Snapshot, error) {
	s.mu.Lock()
	s.loads++
	s.mu.Unlock()
	return s.Store.LoadRun(ctx, runID)
}

func (s *countingProcessStore) AcquireRunLease(ctx context.Context, runID, holder string, ttl time.Duration) (store.LeaseRecord, error) {
	s.mu.Lock()
	s.leases++
	s.mu.Unlock()
	return s.Store.AcquireRunLease(ctx, runID, holder, ttl)
}

func (s *countingProcessStore) counts() (loads, leases int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads, s.leases
}
