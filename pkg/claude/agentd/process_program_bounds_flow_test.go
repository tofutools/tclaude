package agentd_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Program steps run inside the run scope's bounds (tcl-mm9q5n): an
// agent-scoped run's program runs in the agent's cwd inside its sandbox, a
// group-scoped run's takes the group's default spawn profile, and bounds that
// cannot be enforced fail the attempt closed. These tests drive real runs
// through the daemon mux; only the rendering of tclaude's own sandbox is
// swapped, except in the host-sandbox test, which skips without one.

type programBoundsWire struct {
	Scope    string   `json:"scope"`
	Cwd      string   `json:"cwd"`
	Sandbox  string   `json:"sandbox"`
	Profiles []string `json:"profiles"`
	Reason   string   `json:"reason"`
	Error    string   `json:"error"`
}

type programBoundsAttempt struct {
	prepared programBoundsWire
	stdout   string
	outcome  string
	error    string
}

// programBoundsTemplate runs `pwd` once, so the attempt's stdout says where
// it actually ran.
func programBoundsTemplate(id string, run string, args ...string) *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{"next": "where"}},
			"where": {
				Type: model.NodeTypeTask, Next: model.Next{"next": "end"},
				Performer: &model.Performer{Kind: model.PerformerProgram, Profile: "safe", Run: run, Args: args},
			},
			"end": {Type: model.NodeTypeEnd, Result: "success"},
		},
	}
}

type layerWrapCall struct {
	cwd      string
	snapshot *sandboxpolicy.Snapshot
	argv     []string
}

// fakeLayerWrap records each sandbox rendering and runs argv unchanged, or
// refuses with err.
func fakeLayerWrap(t *testing.T, err error) *[]layerWrapCall {
	t.Helper()
	var mu sync.Mutex
	calls := &[]layerWrapCall{}
	t.Cleanup(agentd.SetProcessProgramLayerWrapForTest(func(cwd string, snapshot *sandboxpolicy.Snapshot, argv []string) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		*calls = append(*calls, layerWrapCall{cwd: cwd, snapshot: snapshot, argv: append([]string(nil), argv...)})
		if err != nil {
			return nil, err
		}
		return argv, nil
	}))
	return calls
}

// runProgramBounds starts templateID scoped by scope, waits for its single
// program attempt, and reads back what the attempt recorded.
func runProgramBounds(t *testing.T, f *testharness.Flow, runID, templateID string, scope map[string]any) programBoundsAttempt {
	t.Helper()
	performed := make(chan struct{}, 1)
	t.Cleanup(agentd.SetProcessProgramPerformForTest(func(ctx context.Context, dispatch *executor.Dispatch) (executor.Result, error) {
		result, err := executor.Perform(ctx, dispatch)
		performed <- struct{}{}
		return result, err
	}))
	body := map[string]any{"id": runID, "templateId": templateID, "authorizeProgramProfiles": []string{"safe"}}
	for k, v := range scope {
		body[k] = v
	}
	created := processRuntimeRequest(t, f, http.MethodPost, "/v1/process/runs", body)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	<-performed
	agentd.WaitForProcessRunRuntimeForTest()

	evidence := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/"+runID+"/events", nil)
	require.Equal(t, http.StatusOK, evidence.Code, evidence.Body.String())
	var page processRuntimeEventPage
	testharness.DecodeJSON(t, evidence, &page)
	var attempt programBoundsAttempt
	for _, event := range page.Events {
		switch event.Kind {
		case "program_prepared":
			var payload struct {
				Bounds *programBoundsWire `json:"bounds"`
			}
			require.NoError(t, json.Unmarshal(event.Payload, &payload))
			require.NotNil(t, payload.Bounds, "a scoped attempt records its bounds: %s", event.Payload)
			attempt.prepared = *payload.Bounds
		case "program_observed":
			var payload struct {
				Stdout      string `json:"stdout"`
				Observation struct {
					Outcome string `json:"outcome"`
					Error   string `json:"error"`
				} `json:"observation"`
			}
			require.NoError(t, json.Unmarshal(event.Payload, &payload))
			attempt.stdout = strings.TrimSpace(payload.Stdout)
			attempt.outcome, attempt.error = payload.Observation.Outcome, payload.Observation.Error
		}
	}
	return attempt
}

// contextProgramBounds reads the node's bounds off GET /v1/process/context.
func contextProgramBounds(t *testing.T, f *testharness.Flow, runID, nodeID string) *programBoundsWire {
	t.Helper()
	rec := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/context", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view struct {
		Runs map[string]struct {
			Nodes []struct {
				ID     string             `json:"id"`
				Bounds *programBoundsWire `json:"bounds"`
			} `json:"nodes"`
		} `json:"runs"`
	}
	testharness.DecodeJSON(t, rec, &view)
	run, ok := view.Runs[runID]
	require.True(t, ok, "run %s is in context", runID)
	for _, node := range run.Nodes {
		if node.ID == nodeID {
			return node.Bounds
		}
	}
	t.Fatalf("node %s not projected", nodeID)
	return nil
}

// haveBoundsAgent enrols a grouped agent working in its own directory and
// records the sandbox its current generation launched with.
func haveBoundsAgent(t *testing.T, f *testharness.Flow, group, convID, implementation string, snapshot *sandboxpolicy.Snapshot) (string, string) {
	t.Helper()
	cwd := filepath.Join(testutil.CanonicalTempDir(t), "work")
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	f.HaveAliveSession(convID, "sess-"+convID[:4], "tmux-"+convID[:4], cwd)
	f.HaveMember(group, convID)
	rows, err := db.FindSessionsByConvID(convID)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	for _, row := range rows {
		row.SandboxImplementation = implementation
		row.EffectiveSandbox = snapshot
		require.NoError(t, db.SaveSession(row))
	}
	return agentIDOf(t, convID), cwd
}

func boundsSnapshot(t *testing.T, name string) *sandboxpolicy.Snapshot {
	t.Helper()
	snapshot := sandboxpolicy.EmptySnapshot()
	snapshot.Applied = []sandboxpolicy.AppliedProfile{{Scope: sandboxpolicy.ScopeGroup, ID: 1, Name: name}}
	return &snapshot
}

// Scenario: an agent-scoped run's program step runs in the agent's working
// directory inside the agent's sandbox, and the attempt says so — in its
// evidence and on the run's context projection.
func TestProcessProgramBoundsAgentScopedRunsInAgentSandbox(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, programBoundsTemplate("where", "pwd"))
	f.HaveGroup("bounds-crew")
	agentID, cwd := haveBoundsAgent(t, f, "bounds-crew", "pbag-1111-2222-3333-4444",
		string(sandboxpolicy.ImplementationTclaudeLayer), boundsSnapshot(t, "dev-net"))
	calls := fakeLayerWrap(t, nil)

	attempt := runProgramBounds(t, f, "run_bounds_agent", "where", map[string]any{"agent": agentID})
	assert.Equal(t, programBoundsWire{Scope: "agent", Cwd: cwd, Sandbox: "tclaude-layer",
		Profiles: []string{"dev-net"}, Reason: "the agent's sandbox (tclaude-layer)"}, attempt.prepared)
	assert.Equal(t, string(engine.ProgramSucceeded), attempt.outcome, attempt.error)
	assert.Equal(t, cwd, attempt.stdout, "the program ran in the agent's working directory")
	require.Len(t, *calls, 1, "the program was rendered inside tclaude's sandbox exactly once")
	assert.Equal(t, cwd, (*calls)[0].cwd)
	assert.Equal(t, []string{"pwd"}, (*calls)[0].argv)
	require.NotNil(t, (*calls)[0].snapshot)
	assert.Equal(t, "dev-net", (*calls)[0].snapshot.Applied[0].Name, "the agent's own recorded sandbox")

	shown := processRuntimeRequest(t, f, http.MethodGet, "/v1/process/runs/run_bounds_agent", nil)
	require.Equal(t, http.StatusOK, shown.Code, shown.Body.String())
	var view struct {
		ProgramBounds map[string]programBoundsWire `json:"programBounds"`
	}
	testharness.DecodeJSON(t, shown, &view)
	assert.Equal(t, map[string]programBoundsWire{"where": attempt.prepared}, view.ProgramBounds,
		"process show names where each program node last ran")
}

// Scenario: bounds that cannot be enforced fail the attempt closed — the
// program never starts, and never unconfined instead.
func TestProcessProgramBoundsFailClosedWhenSandboxUnavailable(t *testing.T) {
	f, root := processRuntimeFlow(t)
	f.HaveGroup("bounds-closed")
	agentID, cwd := haveBoundsAgent(t, f, "bounds-closed", "pbfc-1111-2222-3333-4444",
		string(sandboxpolicy.ImplementationTclaudeLayer), boundsSnapshot(t, "dev-net"))
	marker := filepath.Join(cwd, "ran")
	putProcessRuntimeTemplate(t, root, programBoundsTemplate("touch", "touch", marker))
	fakeLayerWrap(t, errors.New("bwrap is not installed"))

	attempt := runProgramBounds(t, f, "run_bounds_closed", "touch", map[string]any{"agent": agentID})
	assert.Equal(t, "tclaude-layer", attempt.prepared.Sandbox)
	assert.Equal(t, string(engine.ProgramFailed), attempt.outcome)
	assert.Contains(t, attempt.error, "cannot be enforced")
	assert.Contains(t, attempt.error, "bwrap is not installed")
	assert.NoFileExists(t, marker, "the program never ran")
}

// Scenario: an agent launched with no OS sandbox has nothing to narrow to:
// its program steps run unconfined in its working directory, disclosed as
// such, and tclaude's sandbox is never rendered.
func TestProcessProgramBoundsUnconfinedAgentRunsInItsCwd(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, programBoundsTemplate("where", "pwd"))
	f.HaveGroup("bounds-open")
	snapshot := sandboxpolicy.EmptySnapshot()
	agentID, cwd := haveBoundsAgent(t, f, "bounds-open", "pbun-1111-2222-3333-4444",
		string(sandboxpolicy.ImplementationOff), &snapshot)
	calls := fakeLayerWrap(t, nil)

	attempt := runProgramBounds(t, f, "run_bounds_open", "where", map[string]any{"agent": agentID})
	assert.Equal(t, programBoundsWire{Scope: "agent", Cwd: cwd, Sandbox: "unconfined",
		Reason: "the agent runs with --sandbox-impl off"}, attempt.prepared)
	assert.Equal(t, string(engine.ProgramSucceeded), attempt.outcome, attempt.error)
	assert.Equal(t, cwd, attempt.stdout)
	assert.Empty(t, *calls)
}

// Scenario: a group-scoped run takes the group's default spawn profile — its
// working directory and the group's sandbox profiles — and a group with no
// default keeps today's behaviour, disclosed as unconfined.
func TestProcessProgramBoundsGroupScopeUsesDefaultSpawnProfile(t *testing.T) {
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, programBoundsTemplate("where", "pwd"))
	f.HaveGroup("bounds-bare")
	f.HaveGroup("bounds-team")
	calls := fakeLayerWrap(t, nil)

	bare := runProgramBounds(t, f, "run_bounds_bare", "where", map[string]any{"group": "bounds-bare"})
	assert.Equal(t, programBoundsWire{Scope: "group", Sandbox: "unconfined",
		Reason: "the group has no default spawn profile"}, bare.prepared)
	assert.Equal(t, string(engine.ProgramSucceeded), bare.outcome, bare.error)
	assert.Empty(t, *calls)

	cwd := filepath.Join(testutil.CanonicalTempDir(t), "team")
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "team-default",
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer)})
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultProfile("bounds-team", "team-default")
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultCwd("bounds-team", cwd)
	require.NoError(t, err)

	team := runProgramBounds(t, f, "run_bounds_team", "where", map[string]any{"group": "bounds-team"})
	assert.Equal(t, programBoundsWire{Scope: "group", Cwd: cwd, Sandbox: "tclaude-layer",
		Reason: `the group's default spawn profile "team-default"`}, team.prepared)
	assert.Equal(t, string(engine.ProgramSucceeded), team.outcome, team.error)
	assert.Equal(t, cwd, team.stdout)
	require.Len(t, *calls, 1)
	assert.Equal(t, cwd, (*calls)[0].cwd)

	projected := contextProgramBounds(t, f, "run_bounds_team", "where")
	require.NotNil(t, projected, "the run's context projection shows where the step ran")
	assert.Equal(t, team.prepared, *projected)
}

// Scenario: on a host where tclaude's own sandbox works, an agent-scoped
// program step really runs inside it, in the agent's working directory.
func TestProcessProgramBoundsHostSandbox(t *testing.T) {
	if err := session.TclaudeLayerServerHostAvailability(); err != nil {
		t.Skipf("tclaude's sandbox is unavailable on this host: %v", err)
	}
	f, root := processRuntimeFlow(t)
	putProcessRuntimeTemplate(t, root, programBoundsTemplate("where", "pwd"))
	f.HaveGroup("bounds-host")
	snapshot := sandboxpolicy.EmptySnapshot()
	agentID, cwd := haveBoundsAgent(t, f, "bounds-host", "pbhs-1111-2222-3333-4444",
		string(sandboxpolicy.ImplementationTclaudeLayer), &snapshot)

	attempt := runProgramBounds(t, f, "run_bounds_host", "where", map[string]any{"agent": agentID})
	assert.Equal(t, "tclaude-layer", attempt.prepared.Sandbox)
	require.Equal(t, string(engine.ProgramSucceeded), attempt.outcome, attempt.error)
	assert.Equal(t, cwd, attempt.stdout)
}
