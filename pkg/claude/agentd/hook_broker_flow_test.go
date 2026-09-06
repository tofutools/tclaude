package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/platform/execution"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TCL-754 flow tests: a `tclaude-layer` agent cannot reach the
// conversation database from inside its mount namespace, so its hook
// callbacks POST the parsed event to agentd and the daemon applies it
// host-side. These scenarios drive that endpoint through the production
// mux and assert on the same read paths a direct callback feeds.
//
// The property that matters is PARITY: a wrapped agent's hooks must land
// on the dashboard exactly like an unwrapped agent's. So the parity test
// below runs one identical event sequence twice — once direct, once
// brokered — and compares the resulting rows rather than asserting
// hand-written expectations, which would drift.

const (
	brokerLayerConv  = "b0000000-1111-2222-3333-444444444444"
	brokerDirectConv = "d0000000-1111-2222-3333-444444444444"
	brokerVictimConv = "5ac10000-1111-2222-3333-444444444444"

	brokerLayerLabel  = "spwn-broker-layer"
	brokerDirectLabel = "spwn-broker-direct"
	brokerVictimLabel = "spwn-broker-victim"

	// The wrapped ancestry the layer produces: the hook callback runs
	// under the harness, which runs under bubblewrap's inner shell, which
	// runs under bwrap, which runs under the pane shell whose pid the
	// sessions row was keyed by at spawn.
	brokerHookPID    = 7100
	brokerHarnessPID = 7101
	brokerInnerShPID = 7102
	brokerBwrapPID   = 7103
	brokerPanePID    = 7104

	brokerVictimPanePID = 7204
)

// layerProcTree models the wrapped ancestry above and returns the caller
// pid a brokered hook would connect from.
func layerProcTree(t *testing.T) int {
	t.Helper()
	t.Cleanup(agentd.SetProcTreeForTest(
		map[int]string{
			brokerHookPID:    "tclaude",
			brokerHarnessPID: "node",
			brokerInnerShPID: "sh",
			brokerBwrapPID:   "bwrap",
			brokerPanePID:    "sh",
		},
		map[int]int{
			brokerHookPID:    brokerHarnessPID,
			brokerHarnessPID: brokerInnerShPID,
			brokerInnerShPID: brokerBwrapPID,
			brokerBwrapPID:   brokerPanePID,
		},
	))
	return brokerHookPID
}

func managedSelection(t *testing.T, sessionID string) (execution.ID, dbSelection) {
	t.Helper()
	identity, err := db.GetSessionExitLaunchIdentity(sessionID)
	require.NoError(t, err)
	executionID, err := execution.ParseID(identity.Generation)
	require.NoError(t, err)
	selection, found, err := db.CurrentConversationSelection(executionID)
	require.NoError(t, err)
	require.True(t, found)
	return executionID, dbSelection{
		Conversation: string(selection.Conversation),
		Reference:    selection.Reference.Value,
		Revision:     int64(selection.Revision),
	}
}

type dbSelection struct {
	Conversation string
	Reference    string
	Revision     int64
}

func TestManagedHookAdmission_RotatesReferenceAndLogicalConversation(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)

	code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{Input: session.HookCallbackInput{
		ConvID: brokerLayerConv, HookEventName: "SessionStart", Source: "startup",
	}})
	require.Equal(t, http.StatusOK, code)
	executionID, first := managedSelection(t, brokerLayerLabel)
	assert.Equal(t, brokerLayerConv, first.Reference)
	assert.EqualValues(t, 1, first.Revision)

	const rotated = "b0000000-1111-2222-3333-555555555555"
	code, _ = postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{Input: session.HookCallbackInput{
		ConvID: rotated, HookEventName: "SessionStart", Source: "startup",
	}})
	require.Equal(t, http.StatusOK, code)
	_, stillFirst := managedSelection(t, brokerLayerLabel)
	assert.Equal(t, first, stillFirst,
		"an unverified startup-labelled rotation must not select a reference")

	clear := session.BrokeredHookRequest{Input: session.HookCallbackInput{
		ConvID: rotated, HookEventName: "SessionStart", Source: "clear",
	}}
	code, _ = postBrokeredHook(t, f, callerPID, clear)
	require.Equal(t, http.StatusOK, code)
	gotExecution, second := managedSelection(t, brokerLayerLabel)
	assert.Equal(t, executionID, gotExecution, "clear stays within one execution")
	assert.Equal(t, rotated, second.Reference)
	assert.EqualValues(t, 2, second.Revision)
	assert.NotEqual(t, first.Conversation, second.Conversation,
		"clear must mint a new logical conversation")

	code, _ = postBrokeredHook(t, f, callerPID, clear)
	require.Equal(t, http.StatusOK, code)
	_, replay := managedSelection(t, brokerLayerLabel)
	assert.Equal(t, second, replay, "the current exact duplicate must not append a revision")

	const compacted = "b0000000-1111-2222-3333-666666666666"
	code, _ = postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{Input: session.HookCallbackInput{
		ConvID: compacted, HookEventName: "SessionStart", Source: "compact",
	}})
	require.Equal(t, http.StatusOK, code)
	_, continued := managedSelection(t, brokerLayerLabel)
	assert.EqualValues(t, 3, continued.Revision)
	assert.Equal(t, second.Conversation, continued.Conversation,
		"compact advances the external reference while preserving logical history")
}

func TestManagedHookAdmission_DelayedClearCannotRestoreSupersededReference(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)

	sendStart := func(convID, source string) {
		t.Helper()
		code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{Input: session.HookCallbackInput{
			ConvID: convID, HookEventName: "SessionStart", Source: source,
		}})
		require.Equal(t, http.StatusOK, code)
	}

	sendStart(brokerLayerConv, "startup")
	sendStart("b0000000-1111-2222-3333-555555555555", "clear")
	const currentRef = "b0000000-1111-2222-3333-666666666666"
	sendStart(currentRef, "clear")
	_, before := managedSelection(t, brokerLayerLabel)
	require.Equal(t, currentRef, before.Reference)
	require.EqualValues(t, 3, before.Revision)

	// This is the original clear-B observation arriving again after clear-C.
	// The transport has no event ID, so it must remain historical rather than
	// being assigned the current revision and treated as a new clear.
	sendStart("b0000000-1111-2222-3333-555555555555", "clear")
	_, after := managedSelection(t, brokerLayerLabel)
	assert.Equal(t, before, after)
	row, err := db.LoadSession(brokerLayerLabel)
	require.NoError(t, err)
	assert.Equal(t, currentRef, row.ConvID, "legacy projection must not roll back either")
}

func TestManagedHookAdmission_RecognizesVersionNamedClaudeMain(t *testing.T) {
	f := newFlow(t)
	const (
		label     = "spwn-version-main"
		convID    = "b0000000-1111-2222-3333-777777777777"
		callerPID = 7400
		hookShPID = 7401
		mainPID   = 7402
		bwrapPID  = 7403
		panePID   = 7404
		tmux      = "tmux-version-main"
	)
	haveLayerSession(t, f, convID, label, tmux, panePID)
	t.Cleanup(agentd.SetProcTreeForTest(
		map[int]string{
			callerPID: "tclaude", hookShPID: "sh", mainPID: "2.1.234",
			bwrapPID: "bwrap", panePID: "sh",
		},
		map[int]int{
			callerPID: hookShPID, hookShPID: mainPID, mainPID: bwrapPID, bwrapPID: panePID,
		},
	))
	code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		ClaimedSessionID: label,
		Input: session.HookCallbackInput{
			ConvID: convID, HookEventName: "SessionStart", Source: "startup",
		},
	})
	require.Equal(t, http.StatusOK, code)
	row, err := db.LoadSession(label)
	require.NoError(t, err)
	assert.Equal(t, mainPID, row.PID)
	_, selection := managedSelection(t, label)
	assert.Equal(t, convID, selection.Reference)
}

func TestManagedHookAdmission_CodexNamespaceMustBeDurablyKnown(t *testing.T) {
	const (
		label  = "spwn-codex-namespace"
		convID = "b0000000-1111-2222-3333-888888888888"
		tmux   = "tmux-codex-namespace"
	)
	setup := func(t *testing.T, recordRoot bool) (*testharness.Flow, int) {
		t.Helper()
		f := newFlow(t)
		f.HaveAliveCodexSession(convID, label, tmux, f.World.HomeDir)
		row, err := db.LoadSession(label)
		require.NoError(t, err)
		row.PID = brokerPanePID
		row.SandboxImplementation = "tclaude-layer"
		row.ExitLaunchGeneration = fmt.Sprintf("%032x", brokerPanePID)
		require.NoError(t, db.SaveSession(row))
		f.World.Tmux.SetPaneIdentityForTest(tmux, "%1", brokerPanePID)
		f.World.Tmux.SetPaneExitGeneration(tmux, row.ExitLaunchGeneration)
		require.NoError(t, db.SetSessionExitLaunchBinding(
			label, row.ExitLaunchGeneration, fmt.Sprintf("%064x", brokerPanePID), "%1"))
		t.Cleanup(agentd.SetProcTreeForTest(
			map[int]string{
				brokerHookPID: "tclaude", brokerHarnessPID: "codex",
				brokerBwrapPID: "bwrap", brokerPanePID: "sh",
			},
			map[int]int{
				brokerHookPID: brokerHarnessPID, brokerHarnessPID: brokerBwrapPID,
				brokerBwrapPID: brokerPanePID,
			},
		))
		t.Cleanup(agentd.SetBrokerProcessInstanceForTest(func(pid int) (string, bool) {
			return fmt.Sprintf("test-process-start:%d:1", pid), pid > 1
		}))
		if recordRoot {
			root := filepath.Join(f.World.HomeDir, "custom-codex-state")
			recordLayerStateStoreIdentity(t, label, row.ExitLaunchGeneration, harness.CodexName, root)
		}
		return f, brokerHookPID
	}

	t.Run("recorded custom root remains a distinct namespace", func(t *testing.T) {
		f, callerPID := setup(t, true)
		// Mutable relaunch defaults belong to a future launch and cannot relabel
		// the already-frozen execution boundary.
		agentID, _, err := db.EnsureAgentForConv(convID, "test")
		require.NoError(t, err)
		futureRoot := filepath.Join(f.World.HomeDir, "future-codex-state")
		futureSource := "CODEX_HOME"
		require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
			Version: db.RelaunchProfileVersion, CodexStateRoot: &futureRoot,
			CodexStateRootSource: &futureSource,
		}))
		t.Setenv("CODEX_HOME", futureRoot)
		code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
			ClaimedSessionID: label,
			Input: session.HookCallbackInput{
				ConvID: convID, HookEventName: "SessionStart", Source: "startup",
			},
		})
		require.Equal(t, http.StatusOK, code)
		executionID, err := execution.ParseID(fmt.Sprintf("%032x", brokerPanePID))
		require.NoError(t, err)
		selection, found, err := db.CurrentConversationSelection(executionID)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "host-path:"+filepath.Join(f.World.HomeDir, "custom-codex-state"),
			selection.Reference.Namespace)
	})

	t.Run("unknown root is not collapsed into default", func(t *testing.T) {
		f, callerPID := setup(t, false)
		before, err := db.LoadSession(label)
		require.NoError(t, err)
		code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
			ClaimedSessionID: label,
			Input: session.HookCallbackInput{
				ConvID: convID, HookEventName: "SessionStart", Source: "startup",
			},
		})
		require.Equal(t, http.StatusOK, code)
		after, err := db.LoadSession(label)
		require.NoError(t, err)
		assert.Equal(t, before.Status, after.Status)
		assert.Equal(t, before.LastHook, after.LastHook)
		executionID, err := execution.ParseID(fmt.Sprintf("%032x", brokerPanePID))
		require.NoError(t, err)
		_, found, err := db.CurrentConversationSelection(executionID)
		require.NoError(t, err)
		assert.False(t, found)
	})
}

func TestManagedHookAdmission_CopilotCustomRootsRemainDistinct(t *testing.T) {
	f := newFlow(t)
	const (
		convA = "c0000000-1111-2222-3333-111111111111"
		convB = "c0000000-1111-2222-3333-222222222222"
	)
	type launched struct {
		label, tmux, conv, root   string
		caller, main, bwrap, pane int
	}
	launches := []launched{
		{label: "spwn-copilot-a", tmux: "tmux-copilot-a", conv: convA,
			root: filepath.Join(f.World.HomeDir, "copilot-a"), caller: 7500, main: 7501, bwrap: 7502, pane: 7503},
		{label: "spwn-copilot-b", tmux: "tmux-copilot-b", conv: convB,
			root: filepath.Join(f.World.HomeDir, "copilot-b"), caller: 7600, main: 7601, bwrap: 7602, pane: 7603},
	}
	names := map[int]string{}
	parents := map[int]int{}
	for _, launch := range launches {
		haveLayerHarnessSession(t, f, launch.conv, launch.label, launch.tmux,
			launch.pane, harness.CopilotName, launch.root)
		names[launch.caller] = "tclaude"
		names[launch.main] = "copilot"
		names[launch.bwrap] = "bwrap"
		names[launch.pane] = "sh"
		parents[launch.caller] = launch.main
		parents[launch.main] = launch.bwrap
		parents[launch.bwrap] = launch.pane
	}
	t.Cleanup(agentd.SetProcTreeForTest(names, parents))
	for _, launch := range launches {
		code, _ := postBrokeredHook(t, f, launch.caller, session.BrokeredHookRequest{
			ClaimedSessionID: launch.label,
			Input: session.HookCallbackInput{
				ConvID: launch.conv, HookEventName: "SessionStart", Source: "startup",
			},
		})
		require.Equal(t, http.StatusOK, code)
		executionID, _ := managedSelection(t, launch.label)
		selection, found, err := db.CurrentConversationSelection(executionID)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "host-path:"+launch.root, selection.Reference.Namespace)
	}
}

func TestManagedHookAdmission_RefusesStaleNestedAndInsufficientEvidence(t *testing.T) {
	t.Run("stale attempt", func(t *testing.T) {
		f := newFlow(t)
		callerPID := layerProcTree(t)
		haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
		identity, err := db.GetSessionExitLaunchIdentity(brokerLayerLabel)
		require.NoError(t, err)
		oldGeneration := identity.Generation

		const successor = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		require.NoError(t, db.SetSessionExitLaunchGeneration(brokerLayerLabel, successor))
		require.NoError(t, db.SetSessionExitLaunchBinding(
			brokerLayerLabel, successor, fmt.Sprintf("%064x", brokerPanePID+1), "%1"))
		f.World.Tmux.SetPaneExitGeneration("tmux-broker-layer", successor)

		before, err := db.LoadSession(brokerLayerLabel)
		require.NoError(t, err)
		code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
			ExitGeneration: oldGeneration,
			Input: session.HookCallbackInput{
				ConvID: brokerLayerConv, HookEventName: "UserPromptSubmit", Prompt: "stale",
			},
		})
		require.Equal(t, http.StatusOK, code)
		after, err := db.LoadSession(brokerLayerLabel)
		require.NoError(t, err)
		assert.Equal(t, before.Status, after.Status)
		assert.Equal(t, before.LastHook, after.LastHook)
	})

	t.Run("nested harness", func(t *testing.T) {
		f := newFlow(t)
		haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
		const nestedPID = 7199
		t.Cleanup(agentd.SetProcTreeForTest(
			map[int]string{
				brokerHookPID: "tclaude", nestedPID: "node", brokerHarnessPID: "node",
				brokerBwrapPID: "bwrap", brokerPanePID: "sh",
			},
			map[int]int{
				brokerHookPID: nestedPID, nestedPID: brokerHarnessPID,
				brokerHarnessPID: brokerBwrapPID, brokerBwrapPID: brokerPanePID,
			},
		))
		before, err := db.LoadSession(brokerLayerLabel)
		require.NoError(t, err)
		code, _ := postBrokeredHook(t, f, brokerHookPID, session.BrokeredHookRequest{Input: session.HookCallbackInput{
			ConvID: "nested-conversation", HookEventName: "SessionStart", Source: "clear",
		}})
		require.Equal(t, http.StatusOK, code)
		after, err := db.LoadSession(brokerLayerLabel)
		require.NoError(t, err)
		assert.Equal(t, before.ConvID, after.ConvID)
		assert.Equal(t, before.LastHook, after.LastHook)
	})

	t.Run("malformed generation", func(t *testing.T) {
		f := newFlow(t)
		callerPID := layerProcTree(t)
		haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
		before, err := db.LoadSession(brokerLayerLabel)
		require.NoError(t, err)
		code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
			ExitGeneration: "not-an-execution",
			Input: session.HookCallbackInput{
				ConvID: brokerLayerConv, HookEventName: "UserPromptSubmit", Prompt: "unproved",
			},
		})
		require.Equal(t, http.StatusOK, code)
		after, err := db.LoadSession(brokerLayerLabel)
		require.NoError(t, err)
		assert.Equal(t, before.Status, after.Status)
		assert.Equal(t, before.LastHook, after.LastHook)
	})
}

func TestManagedHookAdmission_LateDuplicateCannotReviveExitedAttempt(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
	code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{Input: session.HookCallbackInput{
		ConvID: brokerLayerConv, HookEventName: "SessionStart", Source: "startup",
	}})
	require.Equal(t, http.StatusOK, code)
	_, beforeSelection := managedSelection(t, brokerLayerLabel)

	row, err := db.LoadSession(brokerLayerLabel)
	require.NoError(t, err)
	marked, err := db.MarkSessionExitedIfUnchanged(row.ID, row.Status, row.UpdatedAt, "unexpected")
	require.NoError(t, err)
	require.True(t, marked)

	code, _ = postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{Input: session.HookCallbackInput{
		ConvID: brokerLayerConv, HookEventName: "SessionStart", Source: "startup",
	}})
	require.Equal(t, http.StatusOK, code)
	after, err := db.LoadSession(brokerLayerLabel)
	require.NoError(t, err)
	assert.Equal(t, "exited", after.Status)
	_, afterSelection := managedSelection(t, brokerLayerLabel)
	assert.Equal(t, beforeSelection, afterSelection)
}

// haveLayerSession stands up a session row recorded as a tclaude-layer
// launch and keyed by the pane pid, which is what the ancestor walk has
// to cross the bwrap wrappers to reach.
func haveLayerSession(t *testing.T, f *testharness.Flow, conv, label, tmux string, panePID int) {
	haveLayerHarnessSession(t, f, conv, label, tmux, panePID, harness.DefaultName,
		filepath.Join(f.World.HomeDir, ".claude"))
}

func haveLayerHarnessSession(
	t *testing.T,
	f *testharness.Flow,
	conv, label, tmux string,
	panePID int,
	harnessName, stateRoot string,
) {
	t.Helper()
	t.Cleanup(agentd.SetBrokerProcessInstanceForTest(func(pid int) (string, bool) {
		if pid <= 1 {
			return "", false
		}
		return fmt.Sprintf("test-process-start:%d:1", pid), true
	}))
	f.HaveAliveSession(conv, label, tmux, f.World.HomeDir)
	generation := fmt.Sprintf("%032x", panePID)
	row, err := db.LoadSession(label)
	require.NoError(t, err, "LoadSession(%s)", label)
	require.NotNil(t, row, "session row %s should exist", label)
	row.PID = panePID
	row.Harness = harnessName
	row.SandboxImplementation = "tclaude-layer"
	row.ExitLaunchGeneration = generation
	require.NoError(t, db.SaveSession(row), "record the layer launch")
	f.World.Tmux.SetPaneIdentityForTest(tmux, "%1", panePID)
	f.World.Tmux.SetPaneExitGeneration(tmux, generation)
	require.NoError(t, db.SetSessionExitLaunchBinding(
		label, generation, fmt.Sprintf("%064x", panePID), "%1"),
		"bind the durable pane identity")
	recordLayerStateStoreIdentity(t, label, generation, harnessName, stateRoot)
}

func recordLayerStateStoreIdentity(t *testing.T, label, generation, harnessName, root string) {
	t.Helper()
	boundary := session.ExecutionBoundary{
		Version: session.ExecutionBoundaryVersion, LaunchGeneration: generation,
		SandboxImplementation: "tclaude-layer",
		Harness:               session.ExecutionHarness{Name: harnessName},
		OuterLayerRenderInput: &session.TclaudeLayerLaunchSpec{
			Version: session.TclaudeLayerLaunchSpecVersion,
			Contract: session.TclaudeLayerLaunchContract{
				HarnessName: harnessName, StateRoot: root,
			},
		},
		StateStoreIdentity: &harness.StateStoreIdentity{
			Harness: harnessName, Namespace: "host-path:" + root,
			StateRoot: root, Source: "test launch",
		},
	}
	raw, err := json.Marshal(&boundary)
	require.NoError(t, err)
	require.NoError(t, db.SetSessionExecutionBoundary(label, string(raw)))
}

// postBrokeredHook drives POST /v1/whoami/hook as a caller at callerPID.
func postBrokeredHook(t *testing.T, f *testharness.Flow, callerPID int, body session.BrokeredHookRequest) (int, session.BrokeredHookResponse) {
	t.Helper()
	if body.ClaimedSessionID == "" {
		switch callerPID {
		case injHookPID:
			body.ClaimedSessionID = injLabel
		default:
			body.ClaimedSessionID = brokerLayerLabel
		}
	}
	if body.ExitGeneration == "" && body.AckToken == "" {
		identity, err := db.GetSessionExitLaunchIdentity(body.ClaimedSessionID)
		if err == nil {
			body.ExitGeneration = identity.Generation
		}
	}
	req := testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/hook", body)
	req = agentd.AsAgentPeerWithPID(req, "", callerPID)
	rec := testharness.Serve(f.Mux, req)
	var out session.BrokeredHookResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "decode broker response")
	}
	return rec.Code, out
}

// A broker writes hook output into an HTTP response buffer first; that is not
// delivery. Only the sandboxed callback knows whether it subsequently wrote
// those bytes to the harness's stdout, so once-per-generation cadence must
// remain open until its explicit acknowledgement arrives.
func TestHookBroker_StandingOrderCommitRequiresRelayAck(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
	group := f.HaveGroup("standing-order-broker")
	f.HaveMember(group.Name, brokerLayerConv)

	orderID, err := db.InsertStandingOrder(&db.StandingOrder{
		Name:             "broker-ack",
		TargetKind:       db.StandingTargetGroup,
		GroupID:          group.ID,
		Summary:          "Do not claim delivery before stdout relay.",
		TriggerEvent:     db.StandingTriggerSessionStart,
		TriggerSources:   []string{db.StandingSourceStartup},
		Timing:           db.StandingTimingSameContinuation,
		Cadence:          db.StandingCadenceOncePerGeneration,
		Enabled:          true,
		OperatorAuthored: true,
	})
	require.NoError(t, err)

	event := session.BrokeredHookRequest{Input: session.HookCallbackInput{
		ConvID: brokerLayerConv, HookEventName: "SessionStart",
		Source: db.StandingSourceStartup, Cwd: f.World.HomeDir,
	}}
	code, resp := postBrokeredHook(t, f, callerPID, event)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, resp.Stdout, "Do not claim delivery")
	require.NotEmpty(t, resp.AckToken)

	latest, err := db.LatestStandingDelivery(orderID)
	require.NoError(t, err)
	assert.Nil(t, latest, "buffering an HTTP response is not harness delivery")
	delivered, err := db.StandingOrderDeliveredInEpoch(
		orderID, 1, brokerLayerConv, brokerLayerConv)
	require.NoError(t, err)
	assert.False(t, delivered, "a failed/disconnected relay must leave cadence retryable")

	code, _ = postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		ClaimedSessionID: brokerLayerLabel,
		AckToken:         resp.AckToken,
	})
	require.Equal(t, http.StatusOK, code)
	latest, err = db.LatestStandingDelivery(orderID)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, db.StandingOutcomeDelivered, latest.Outcome)

	code, _ = postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		ClaimedSessionID: brokerLayerLabel,
		AckToken:         resp.AckToken,
	})
	assert.Equal(t, http.StatusConflict, code, "an acknowledgement is one-shot")
}

// A stdout write failure is an explicit negative acknowledgement: it must
// release the cadence lock without recording delivery, so the next boundary
// can retry immediately rather than waiting for the acknowledgement TTL.
func TestHookBroker_StandingOrderRelayFailureReleasesWithoutCommit(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
	group := f.HaveGroup("standing-order-relay-failure")
	f.HaveMember(group.Name, brokerLayerConv)

	orderID, err := db.InsertStandingOrder(&db.StandingOrder{
		Name:             "broker-relay-failure",
		TargetKind:       db.StandingTargetGroup,
		GroupID:          group.ID,
		Summary:          "Retry after a failed stdout relay.",
		TriggerEvent:     db.StandingTriggerSessionStart,
		TriggerSources:   []string{db.StandingSourceStartup},
		Timing:           db.StandingTimingSameContinuation,
		Cadence:          db.StandingCadenceOncePerGeneration,
		Enabled:          true,
		OperatorAuthored: true,
	})
	require.NoError(t, err)

	event := session.BrokeredHookRequest{Input: session.HookCallbackInput{
		ConvID: brokerLayerConv, HookEventName: "SessionStart",
		Source: db.StandingSourceStartup, Cwd: f.World.HomeDir,
	}}
	code, first := postBrokeredHook(t, f, callerPID, event)
	require.Equal(t, http.StatusOK, code)
	require.NotEmpty(t, first.AckToken)

	code, _ = postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		ClaimedSessionID: brokerLayerLabel,
		AckToken:         first.AckToken,
		RelayFailed:      true,
	})
	require.Equal(t, http.StatusOK, code)
	latest, err := db.LatestStandingDelivery(orderID)
	require.NoError(t, err)
	assert.Nil(t, latest, "a failed relay must not be recorded as delivery")

	code, retry := postBrokeredHook(t, f, callerPID, event)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, retry.Stdout, "Retry after a failed stdout relay")
	require.NotEmpty(t, retry.AckToken,
		"the failed relay must release its lock so the next boundary can retry")

	code, _ = postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		ClaimedSessionID: brokerLayerLabel,
		AckToken:         retry.AckToken,
	})
	require.Equal(t, http.StatusOK, code)
	latest, err = db.LatestStandingDelivery(orderID)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, db.StandingOutcomeDelivered, latest.Outcome)
}

// TestHookBroker_ParityWithDirectCallback is the acceptance property from
// TCL-754: a tclaude-layer agent has hook parity with a harness-builtin
// one. Two sessions get the same event sequence by the two different
// routes; every asserted surface must agree.
func TestHookBroker_ParityWithDirectCallback(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	callerPID := layerProcTree(t)

	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
	f.HaveAliveSession(brokerDirectConv, brokerDirectLabel, "tmux-broker-direct", f.World.HomeDir)

	events := []session.HookCallbackInput{
		{HookEventName: "SessionStart", Source: "startup"},
		{HookEventName: "UserPromptSubmit", Prompt: "do the thing"},
		{HookEventName: "PostToolUse", ToolName: "Edit"},
		{HookEventName: "Stop"},
	}

	for _, base := range events {
		// Brokered: the layer agent's callback hands the event to agentd.
		// Note it sends NO session id it could be trusted on — the daemon
		// resolves the row from the caller's ancestry.
		brokered := base
		brokered.ConvID = brokerLayerConv
		brokered.Cwd = f.World.HomeDir
		code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{Input: brokered})
		require.Equal(t, http.StatusOK, code, "brokered %s should be applied", base.HookEventName)

		// Direct: the harness-builtin agent's callback writes for itself.
		direct := base
		direct.ConvID = brokerDirectConv
		direct.Cwd = f.World.HomeDir
		require.NoError(t, session.ApplyHook(direct, brokerDirectLabel),
			"direct %s should be applied", base.HookEventName)
	}

	layer, err := session.LoadSessionState(brokerLayerLabel)
	require.NoError(t, err)
	builtin, err := session.LoadSessionState(brokerDirectLabel)
	require.NoError(t, err)

	assert.Equal(t, builtin.Status, layer.Status,
		"a wrapped agent's status must track exactly like an unwrapped one's")
	assert.Equal(t, brokerLayerConv, layer.ConvID,
		"the brokered SessionStart must stamp the conv-id onto the resolved row")
	assert.False(t, layer.LastHook.IsZero(), "brokered events must stamp last_hook")

	// The dashboard read path, not just the row: both agents must render
	// identically on the surface the human actually looks at.
	f.HaveGroup("brokersquad")
	f.HaveMember("brokersquad", brokerLayerConv)
	f.HaveMember("brokersquad", brokerDirectConv)
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	layerRow := findDashMember(snap, "brokersquad", brokerLayerConv)
	builtinRow := findDashMember(snap, "brokersquad", brokerDirectConv)
	require.NotNil(t, layerRow, "the wrapped agent must appear on the dashboard")
	require.NotNil(t, builtinRow, "the unwrapped agent must appear on the dashboard")
	assert.Equal(t, builtinRow.State.Status, layerRow.State.Status,
		"dashboard status must match between a brokered and a direct agent")
}

// The client-side oversize test proves trimming SETS PayloadTrimmed. This
// broker flow proves the other half of the production composition: agentd
// preserves that evidence through PrepareHookEvent and the standing-order
// evaluator records "could not evaluate" rather than a false clean miss.
func TestHookBroker_TrimEvidenceReachesStandingOrderLedger(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel,
		"tmux-broker-layer", brokerPanePID)

	targetAgent, _, err := db.EnsureAgentForConv(brokerLayerConv, "test")
	require.NoError(t, err)
	require.NotEmpty(t, targetAgent)
	orderID, err := db.InsertStandingOrder(&db.StandingOrder{
		Name:             "trim-evidence",
		TargetKind:       db.StandingTargetConv,
		TargetAgent:      targetAgent,
		Summary:          "Review the tool input before proceeding.",
		TriggerEvent:     db.StandingTriggerToolBefore,
		MatchField:       db.StandingMatchFieldToolInput,
		MatchRegex:       "deploy",
		Timing:           db.StandingTimingSameContinuation,
		Cadence:          db.StandingCadenceAlways,
		Enabled:          true,
		OperatorAuthored: true,
	})
	require.NoError(t, err)

	code, response := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		Input: session.HookCallbackInput{
			ConvID:         brokerLayerConv,
			HookEventName:  "PreToolUse",
			ToolName:       "Bash",
			Cwd:            f.World.HomeDir,
			PayloadTrimmed: true,
		},
	})
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, response.Stdout)
	assert.Empty(t, response.AckToken, "no model-visible delivery waits for an ACK")

	latest, err := db.LatestStandingDelivery(orderID)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, db.StandingOutcomeNotEvaluatedTrimmed, latest.Outcome)
	assert.NotEqual(t, db.StandingOutcomeNoMatch, latest.Outcome)
}

// TestHookBroker_IdentityComesFromAncestryNotThePayload pins finding 3 of
// the inventory: the caller's TCLAUDE_SESSION_ID is a cross-check, never
// the authority. A wrapped agent that names another agent's session must
// be refused outright rather than quietly writing its own row (which
// would hide the attempt) or the victim's (which would be the bug).
func TestHookBroker_IdentityComesFromAncestryNotThePayload(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)

	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
	haveLayerSession(t, f, brokerVictimConv, brokerVictimLabel, "tmux-broker-victim", brokerVictimPanePID)

	victimBefore, err := session.LoadSessionState(brokerVictimLabel)
	require.NoError(t, err)

	code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		Input: session.HookCallbackInput{
			ConvID:        brokerVictimConv,
			HookEventName: "SessionStart",
			Source:        "startup",
			Cwd:           f.World.HomeDir,
		},
		ClaimedSessionID: brokerVictimLabel,
	})
	assert.Equal(t, http.StatusForbidden, code,
		"a claimed session id that disagrees with the resolved row must be refused")

	victimAfter, err := session.LoadSessionState(brokerVictimLabel)
	require.NoError(t, err)
	assert.Equal(t, victimBefore.ConvID, victimAfter.ConvID,
		"the victim's conv-id must be untouched")
	assert.Equal(t, victimBefore.LastHook, victimAfter.LastHook,
		"the victim's row must not record a hook it never fired")
}

// TestHookBroker_RefusesCallersItCannotPlace is the fail-closed half: a
// caller whose ancestry reaches no recorded session row gets nothing,
// rather than falling back to anything the request asserts about itself.
func TestHookBroker_RefusesCallersItCannotPlace(t *testing.T) {
	f := newFlow(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)

	// A harness-named process with no recorded ancestry at all.
	const orphanPID = 8100
	t.Cleanup(agentd.SetProcTreeForTest(
		map[int]string{orphanPID: "node"},
		map[int]int{},
	))

	code, _ := postBrokeredHook(t, f, orphanPID, session.BrokeredHookRequest{
		Input: session.HookCallbackInput{
			ConvID:        brokerLayerConv,
			HookEventName: "SessionStart",
			Source:        "startup",
		},
		ClaimedSessionID: brokerLayerLabel,
	})
	assert.Equal(t, http.StatusForbidden, code,
		"an unplaceable caller must not be able to name its own session")
}

// TestHookBroker_TranscriptPathIsScopedToTheCallersOwnRollout wires the
// sanitizer into the handler. Without this the transcript gate is only
// unit-tested, and deleting the call site would leave the suite green —
// which for the PR's one cross-agent-read defence is not good enough.
//
// A wrapped Codex agent names a PEER's rollout file. The daemon must drop
// the path, so the peer's transcript is never opened and never lands in
// this caller's conversation index.
func TestHookBroker_TranscriptPathIsScopedToTheCallersOwnRollout(t *testing.T) {
	// conv_index's upsert keeps the first full_path it sees, so the two
	// cases must not share a conversation — otherwise "the peer path was
	// not recorded" would be true for the wrong reason.
	//
	// The transcript path is also consumed only on CODEX paths, so the
	// caller has to be a Codex session for either case to exercise
	// anything at all.
	brokerTranscript := func(t *testing.T, rolloutConv string) (*testharness.Flow, string) {
		t.Helper()
		f := newFlow(t)

		f.HaveAliveCodexSession(brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", f.World.HomeDir)
		codexRoot := filepath.Join(f.World.HomeDir, ".codex")
		row, err := db.LoadSession(brokerLayerLabel)
		require.NoError(t, err)
		require.NotNil(t, row)
		row.PID = brokerPanePID
		row.SandboxImplementation = "tclaude-layer"
		row.ExitLaunchGeneration = fmt.Sprintf("%032x", brokerPanePID)
		require.NoError(t, db.SaveSession(row))
		f.World.Tmux.SetPaneIdentityForTest("tmux-broker-layer", "%1", brokerPanePID)
		f.World.Tmux.SetPaneExitGeneration("tmux-broker-layer", row.ExitLaunchGeneration)
		require.NoError(t, db.SetSessionExitLaunchBinding(
			brokerLayerLabel, row.ExitLaunchGeneration,
			fmt.Sprintf("%064x", brokerPanePID), "%1"))
		recordLayerStateStoreIdentity(t, brokerLayerLabel, row.ExitLaunchGeneration,
			harness.CodexName, codexRoot)
		t.Cleanup(agentd.SetBrokerProcessInstanceForTest(func(pid int) (string, bool) {
			return fmt.Sprintf("test-process-start:%d:1", pid), pid > 1
		}))

		t.Cleanup(agentd.SetProcTreeForTest(
			map[int]string{
				brokerHookPID: "tclaude", brokerHarnessPID: "codex",
				brokerBwrapPID: "bwrap", brokerPanePID: "sh",
			},
			map[int]int{
				brokerHookPID: brokerHarnessPID, brokerHarnessPID: brokerBwrapPID,
				brokerBwrapPID: brokerPanePID,
			},
		))

		sessions := filepath.Join(f.World.HomeDir, ".codex", "sessions", "2026", "07", "26")
		require.NoError(t, os.MkdirAll(sessions, 0o755))
		rollout := filepath.Join(sessions, "rollout-2026-07-26T09-00-00-"+rolloutConv+".jsonl")
		require.NoError(t, os.WriteFile(rollout, []byte("{}\n"), 0o600))

		code, _ := postBrokeredHook(t, f, brokerHookPID, session.BrokeredHookRequest{
			Input: session.HookCallbackInput{
				ConvID:         brokerLayerConv,
				HookEventName:  "Stop",
				Cwd:            f.World.HomeDir,
				TranscriptPath: rollout,
			},
		})
		require.Equal(t, http.StatusOK, code, "the event is applied; only the path may be refused")
		return f, rollout
	}

	// The positive case is what gives the negative one teeth: without it, a
	// sanitizer that dropped EVERY transcript path would pass just as well.
	t.Run("its own rollout is recorded", func(t *testing.T) {
		_, rollout := brokerTranscript(t, brokerLayerConv)
		idx, err := db.GetConvIndex(brokerLayerConv)
		require.NoError(t, err)
		require.NotNil(t, idx, "the caller's own rollout must be indexed")
		assert.Equal(t, rollout, idx.FullPath,
			"a session's own rollout is legitimate telemetry and must survive the broker")
	})

	t.Run("a peer's rollout is refused", func(t *testing.T) {
		_, rollout := brokerTranscript(t, brokerVictimConv)
		idx, err := db.GetConvIndex(brokerLayerConv)
		require.NoError(t, err)
		if idx != nil {
			assert.NotEqual(t, rollout, idx.FullPath,
				"a peer's rollout must never be recorded as this conversation's transcript")
			assert.NotEqual(t, filepath.Dir(rollout), idx.ProjectDir,
				"nor may its directory become this conversation's project dir")
		}
	})
}

// TestHookBroker_RejectsPathTraversingConvID pins the second payload field
// that resolves into a host path: the conv-id is joined into the
// transcript path the /clear migration scans, and filepath.Join cleans
// ".." segments, so an unvalidated one walks out of the projects tree.
func TestHookBroker_RejectsPathTraversingConvID(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)

	for _, hostile := range []string{
		"../../../tmp/aaaaaaaa-1111-2222-3333-444444444444",
		"..",
		"sub/dir",
	} {
		code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
			Input: session.HookCallbackInput{
				ConvID:        hostile,
				HookEventName: "SessionStart",
				Source:        "clear",
				Cwd:           f.World.HomeDir,
			},
		})
		assert.Equal(t, http.StatusBadRequest, code,
			"a conv-id that is not a single path-safe segment must be refused: %q", hostile)
	}
}

// TestHookBroker_PreCompactDecisionIsRelayed proves the one hook event
// whose OUTPUT matters survives the round trip. PreCompact may answer
// {"decision":"block"} to refuse an early auto-compaction; if the broker
// swallowed that, a wrapped agent would silently lose the guard.
func TestHookBroker_PreCompactDecisionIsRelayed(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)

	// The guard is opt-in, so turn it on, then seed a context snapshot at
	// the 200K boundary of a 1M window — the headline case the guard
	// exists to refuse.
	//
	// Scope note: this proves the DECISION SURVIVES THE ROUND TRIP, and
	// nothing about how the snapshot got there. The snapshot the guard
	// judges from is written only by the status line, which is brokered
	// through its own endpoint; seeding it directly here keeps this test
	// about the relay rather than about that path.
	cfg := config.DefaultConfig()
	cfg.PreCompactGuard = &config.PreCompactGuardConfig{Enabled: true}
	require.NoError(t, config.Save(cfg))
	require.NoError(t, db.UpdateContextSnapshot(brokerLayerLabel, 20, 1, 0, 1_000_000))

	code, resp := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		Input: session.HookCallbackInput{
			ConvID:        brokerLayerConv,
			HookEventName: "PreCompact",
			Trigger:       "auto",
		},
	})
	require.Equal(t, http.StatusOK, code)

	var dec struct {
		Decision string `json:"decision"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Stdout), &dec),
		"the guard's decision document must be relayed verbatim, got %q", resp.Stdout)
	assert.Equal(t, "block", dec.Decision,
		"the guard's verdict must survive the round trip")
}
