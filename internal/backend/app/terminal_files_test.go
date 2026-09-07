package app_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

type fileProvider struct {
	*fakeProvider
	fileRuntime *fileRuntime
}
type fileRuntime struct {
	*fakeRuntime
	root    string
	calls   int
	before  func()
	unknown bool
}
type filePrepared struct {
	ports.PreparedAttempt
	runtime *fileRuntime
}

func (p *fileProvider) Prepare(ctx context.Context, in ports.PreparationRequest) (ports.PreparedAttempt, error) {
	a, err := p.fakeProvider.Prepare(ctx, in)
	return &filePrepared{a, p.fileRuntime}, err
}
func (p *fileProvider) Recover(ctx context.Context, in ports.RecoveryRequest) (ports.RecoveryResult, error) {
	r, err := p.fakeProvider.Recover(ctx, in)
	r.Runtime = p.fileRuntime
	return r, err
}
func (p *filePrepared) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	r, err := p.PreparedAttempt.Release(ctx, permit)
	r.Runtime = p.runtime
	return r, err
}
func (r *fileRuntime) StageTerminalFile(ctx context.Context, in ports.StageTerminalFileRequest) (ports.StageTerminalFileResult, error) {
	r.calls++
	if r.before != nil {
		r.before()
	}
	out, err := host.StageTerminalFile(ctx, r.root, r.id, in)
	if r.unknown && out.Disposition == ports.EffectAccepted {
		out = ports.StageTerminalFileResult{Disposition: ports.EffectUnknown}
	}
	return out, err
}

func TestTerminalFilePublicationIsExactDurableAndAuthorityChecked(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backend.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	fake := newFakeProvider()
	runtime := &fileRuntime{fakeRuntime: fake.runtime, root: t.TempDir()}
	provider := &fileProvider{fake, runtime}
	service := app.New(store, providers.NewRegistry(provider))
	operator := model.OperatorPrincipal()
	target := createAgent(t, ctx, service, operator, "target")
	caller := createAgent(t, ctx, service, operator, "caller")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: target.ID, ExpectedRevision: target.Revision}}})
	require.NoError(t, err)
	grant, err := service.PutGrant(ctx, app.PutGrantRequest{Principal: operator, Grant: model.AuthorityGrant{ID: "stage", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: caller.ID}, Action: model.ActionStageTerminalFile, Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: launched.Execution.ID}}})
	require.NoError(t, err)
	req := app.StageTerminalFileRequest{Context: effect(model.AgentPrincipal(caller.ID), "file"), ExecutionID: launched.Execution.ID, Filename: "diagram.png", Content: []byte("user bytes")}
	result, err := service.StageTerminalFile(ctx, req)
	require.NoError(t, err, "%+v / %+v", result, launched.Execution)
	require.Equal(t, model.OperationSucceeded, result.Operation.State)
	require.NotEmpty(t, result.File.SHA256)
	content, err := os.ReadFile(result.File.NativePath)
	require.NoError(t, err)
	require.Equal(t, req.Content, content)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	service = app.New(store, providers.NewRegistry(provider))
	repeated, err := service.StageTerminalFile(ctx, req)
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
	require.Equal(t, result.File, repeated.File)
	require.Equal(t, 1, runtime.calls)
	changed := req
	changed.Content = []byte("changed")
	_, err = service.StageTerminalFile(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	changed = req
	changed.Filename = "other.png"
	_, err = service.StageTerminalFile(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	require.NoError(t, service.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: operator, GrantID: grant.Grant.ID, ExpectedRevision: grant.Grant.Revision}))
	_, err = service.StageTerminalFile(ctx, req)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	require.Equal(t, 1, runtime.calls)
	_, err = service.Recover(ctx, app.RecoverRequest{Principal: operator})
	require.NoError(t, err)
	// Revoke after admission but before the host's publication boundary.
	grant, err = service.PutGrant(ctx, app.PutGrantRequest{Principal: operator, Grant: model.AuthorityGrant{ID: "stage-new", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: caller.ID}, Action: model.ActionStageTerminalFile, Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: launched.Execution.ID}}})
	require.NoError(t, err)
	runtime.before = func() {
		require.NoError(t, service.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: operator, GrantID: grant.Grant.ID, ExpectedRevision: grant.Grant.Revision}))
	}
	req.Context.RequestID = "revoked-at-boundary"
	refused, err := service.StageTerminalFile(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	require.Equal(t, model.OperationRefused, refused.Operation.State)
	require.Empty(t, refused.File.NativePath)
	files, err := os.ReadDir(filepath.Join(runtime.root, "uploads"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	runtime.before = nil
	runtime.unknown = true
	req.Context = effect(operator, "unknown-file")
	unknown, err := service.StageTerminalFile(ctx, req)
	require.ErrorIs(t, err, app.ErrUncertain, "%+v", unknown)
	require.Equal(t, model.OperationUncertain, unknown.Operation.State)
	require.Empty(t, unknown.File.NativePath)
	retry, err := service.StageTerminalFile(ctx, req)
	require.ErrorIs(t, err, app.ErrUncertain)
	require.True(t, retry.Repeated)
	require.Equal(t, 3, runtime.calls)
	execution, err := store.Execution(ctx, launched.Execution.ID)
	require.NoError(t, err)
	require.Equal(t, model.ExecutionRunning, execution.State)
	_, err = service.Stop(ctx, app.StopRequest{RequestContext: effect(operator, "stop"), ExecutionID: execution.ID})
	require.NoError(t, err)
	req.Context.RequestID = "after-stop"
	_, err = service.StageTerminalFile(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	require.Equal(t, 3, runtime.calls)
}

func TestTerminalFileUnsupportedAndInterruptedAdmissionsNeverReplay(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	provider := newFakeProvider()
	service := app.New(store, providers.NewRegistry(provider))
	operator := model.OperatorPrincipal()
	target := createAgent(t, ctx, service, operator, "target")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: target.ID, ExpectedRevision: target.Revision}}})
	require.NoError(t, err)
	req := app.StageTerminalFileRequest{Context: effect(operator, "unsupported"), ExecutionID: launched.Execution.ID, Filename: "file.txt", Content: []byte("content")}
	refused, err := service.StageTerminalFile(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	require.Equal(t, "file_unsupported", refused.Operation.ResultCode)
	retry, err := service.StageTerminalFile(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	require.True(t, retry.Repeated)
	require.Equal(t, refused.Operation.ID, retry.Operation.ID)
	// A crash after admission has no effect receipt. Retrying reports uncertainty
	// without asking a provider to publish a second time.
	pending := refused.Operation
	pending.ID = "interrupted-upload"
	pending.RequestID = "interrupted"
	pending.State = model.OperationAdmitted
	pending.Revision = 1
	pending.ResultCode = ""
	pending.Detail = ""
	file := refused.File
	file.OperationID = pending.ID
	_, err = store.AdmitTerminalFile(ctx, app.TerminalFileAdmission{Operation: pending, File: file, Authority: model.AuthorityRequest{Principal: operator, Action: model.ActionStageTerminalFile, Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: launched.Execution.ID}}})
	require.NoError(t, err)
	req.Context.RequestID = pending.RequestID
	retry, err = service.StageTerminalFile(ctx, req)
	require.ErrorIs(t, err, app.ErrUncertain)
	require.True(t, retry.Repeated)
	require.Equal(t, model.OperationAdmitted, retry.Operation.State)
	for _, filename := range []string{"../escape", "bad\x00name", "", "a/b"} {
		req.Context.RequestID = "invalid"
		req.Filename = filename
		_, err = service.StageTerminalFile(ctx, req)
		require.ErrorIs(t, err, app.ErrInvalid)
	}
}
