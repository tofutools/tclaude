package app_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestDirectoryTrustDeferredWorkRetainsProofAcrossRestart(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := sqlite.Open(database)
	require.NoError(t, err)
	parentProvider := &peerMessagingProvider{newFakeProvider()}
	wrapped := &directoryTrustAdmissionRaceStore{Store: store}
	service := app.New(wrapped, providers.NewRegistry(parentProvider)).WithDirectoryWriteProof(host.DirectoryProof{})
	op := model.OperatorPrincipal()
	root := t.TempDir()
	desired := model.DesiredConfiguration{Harness: "claude", Model: "worker", WorkingDirectory: root, Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	parent, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "parent", Name: "Parent", Desired: desired})
	require.NoError(t, err)
	parentRun, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(op, "parent"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: parent.Agent.ID, ExpectedRevision: parent.Agent.Revision}}})
	require.NoError(t, err)
	desired.TrustDirectory = true
	_, err = service.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "child", Name: "Child", Desired: desired})
	require.NoError(t, err)
	caller := model.AgentPrincipal(parent.Agent.ID)
	caller.ExecutionID = parentRun.Execution.ID
	for _, grant := range []model.AuthorityGrant{
		{ID: "start_work", Action: model.ActionStartWork, Resource: model.ResourceSelector{Kind: model.ResourceWorkRun, WorkRunID: "work"}},
		{ID: "launch_child", Action: model.ActionLaunch, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: "child"}, Bounds: model.ConfigurationBounds{Harnesses: []string{"claude"}, Models: []string{"worker"}, WorkingDirectoryRoots: []string{root}, ApprovalModes: []model.ApprovalMode{desired.Approval}, SandboxModes: []model.SandboxMode{desired.Sandbox}}},
	} {
		grant.Subject = model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: parent.Agent.ID}
		_, err = service.PutGrant(ctx, app.PutGrantRequest{Principal: op, Grant: grant})
		require.NoError(t, err)
	}
	graph := model.WorkGraph{CompilerVersion: "v1", EntryNodeID: "task", Nodes: []model.WorkNode{{ID: "task", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "child", ContextPolicy: model.AgentContextFresh, Brief: "Do the work"}}}}}
	req := app.StartProcessRequest{Context: effect(caller, "work"), ID: "work", Start: model.WorkStart{InlineGraph: &graph, Deadline: time.Now().Add(time.Hour)}}
	_, err = service.StartProcess(ctx, req)
	var challenge *app.DirectoryProofRequired
	require.ErrorAs(t, err, &challenge)
	_, err = store.WorkRun(ctx, "work")
	require.ErrorIs(t, err, app.ErrNotFound)
	for _, dir := range challenge.Directories {
		require.NoError(t, os.WriteFile(filepath.Join(dir, challenge.Filename), nil, 0600))
	}
	req.Context.WriteProofToken = challenge.Token
	wrapped.before = func() {
		child, readErr := store.Agent(ctx, "child")
		require.NoError(t, readErr)
		changed := child.Desired
		changed.WorkingDirectory = t.TempDir()
		_, updateErr := service.UpdateAgent(ctx, app.UpdateAgentRequest{Context: op, ID: child.ID, ExpectedRevision: child.Revision, Name: child.Name, Desired: changed})
		require.NoError(t, updateErr)
	}
	_, err = service.StartProcess(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict, "a concurrent directory edit must not publish stale caller proof")
	_, err = store.WorkRun(ctx, "work")
	require.ErrorIs(t, err, app.ErrNotFound)
	child, err := store.Agent(ctx, "child")
	require.NoError(t, err)
	_, err = service.UpdateAgent(ctx, app.UpdateAgentRequest{Context: op, ID: child.ID, ExpectedRevision: child.Revision, Name: child.Name, Desired: desired})
	require.NoError(t, err)
	req.Context.WriteProofToken = ""
	_, err = service.StartProcess(ctx, req)
	require.ErrorAs(t, err, &challenge)
	for _, dir := range challenge.Directories {
		require.NoError(t, os.WriteFile(filepath.Join(dir, challenge.Filename), nil, 0600))
	}
	req.Context.WriteProofToken = challenge.Token
	started, err := service.StartProcess(ctx, req)
	require.NoError(t, err)
	require.NotEmpty(t, started.Run.DirectoryTrust.Nodes["task"].ProvenDirectories)
	public, err := json.Marshal(started)
	require.NoError(t, err)
	require.NotContains(t, string(public), "ProvenDirectories")
	for _, dir := range challenge.Directories {
		require.NoFileExists(t, filepath.Join(dir, challenge.Filename))
	}
	require.NoError(t, store.Close())
	store, err = sqlite.Open(database)
	require.NoError(t, err)
	defer store.Close()
	provider := &trustWorkProvider{preparedWorkProvider: &preparedWorkProvider{}}
	service = app.New(store, providers.NewRegistry(provider)).WithDirectoryWriteProof(host.DirectoryProof{})
	req.Context.WriteProofToken = ""
	repeated, err := service.StartProcess(ctx, req)
	require.NoError(t, err)
	require.Equal(t, started.Run.DirectoryTrust.Nodes, repeated.Run.DirectoryTrust.Nodes)
	require.Empty(t, provider.preparations)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	require.Len(t, provider.preparations, 1)
	require.True(t, provider.preparation.Spec.TrustDirectory)
	physical, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.Equal(t, physical, provider.preparation.Spec.WorkingDirectory)
	changed := req
	changed.Start.Deadline = changed.Start.Deadline.Add(time.Minute)
	_, err = service.StartProcess(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
}

type trustWorkProvider struct{ *preparedWorkProvider }

func (*trustWorkProvider) Name() string { return "claude" }
func (p *trustWorkProvider) Prepare(ctx context.Context, req ports.PreparationRequest) (ports.PreparedAttempt, error) {
	attempt, err := p.preparedWorkProvider.Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	return &trustWorkAttempt{attempt.(*preparedWorkAttempt)}, nil
}

type trustWorkAttempt struct{ *preparedWorkAttempt }

func (p *trustWorkAttempt) Describe() ports.PreparedDescription {
	d := p.preparedWorkAttempt.Describe()
	d.Evidence.Provider = "claude"
	return d
}
func (p *trustWorkAttempt) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	result, err := p.preparedWorkAttempt.Release(ctx, permit)
	result.Evidence.Provider = "claude"
	if result.Runtime != nil {
		result.Runtime = &trustWorkRuntime{result.Runtime}
	}
	return result, err
}

type trustWorkRuntime struct{ ports.Runtime }

func (r *trustWorkRuntime) Observe(ctx context.Context) (ports.Observation, error) {
	v, err := r.Runtime.Observe(ctx)
	v.Evidence.Provider = "claude"
	return v, err
}

// Only the concurrent external edit is doubled; publication remains real SQLite.
type directoryTrustAdmissionRaceStore struct {
	app.Store
	before func()
}

func (s *directoryTrustAdmissionRaceStore) CreateGraphWorkRun(ctx context.Context, run model.WorkRun, windows []model.DecisionWindow) (app.WorkRunRecord, bool, error) {
	if s.before != nil {
		callback := s.before
		s.before = nil
		callback()
	}
	return s.Store.CreateGraphWorkRun(ctx, run, windows)
}
