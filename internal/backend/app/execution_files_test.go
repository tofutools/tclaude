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
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

type revokingFileReader struct {
	host.DirectoryBrowser
	after func()
}

func (r revokingFileReader) ReadExecutionFile(ctx context.Context, in ports.ExecutionFileReadRequest) (ports.ExecutionFileContent, error) {
	out, err := r.DirectoryBrowser.ReadExecutionFile(ctx, in)
	r.after()
	return out, err
}
func TestExecutionFileReadRequiresCurrentExactAuthorityBeforeReturningBytes(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	provider := newFakeProvider()
	service := testService(store, provider).WithDirectoryBrowser(host.DirectoryBrowser{})
	operator := model.OperatorPrincipal()
	target := createAgent(t, ctx, service, operator, "target")
	caller := createAgent(t, ctx, service, operator, "caller")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: target.ID, ExpectedRevision: target.Revision}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(target.Desired.WorkingDirectory, "report.txt"), []byte("private report"), 0600))
	req := app.ReadExecutionFileRequest{Principal: model.AgentPrincipal(caller.ID), ExecutionID: launched.Execution.ID, Path: "report.txt"}
	_, err = service.ReadExecutionFile(ctx, req)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	grant, err := service.PutGrant(ctx, app.PutGrantRequest{Principal: operator, Grant: model.AuthorityGrant{ID: "read", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: caller.ID}, Action: model.ActionReadExecutionFile, Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: launched.Execution.ID}}})
	require.NoError(t, err)
	file, err := service.ReadExecutionFile(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "private report", string(file.Content))
	service.WithDirectoryBrowser(revokingFileReader{after: func() {
		require.NoError(t, service.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: operator, GrantID: grant.Grant.ID, ExpectedRevision: grant.Grant.Revision}))
	}})
	file, err = service.ReadExecutionFile(ctx, req)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	require.Empty(t, file.Content)
	service.WithDirectoryBrowser(host.DirectoryBrowser{})
	req.Principal = operator
	_, err = service.Stop(ctx, app.StopRequest{RequestContext: effect(operator, "stop"), ExecutionID: launched.Execution.ID})
	require.NoError(t, err)
	file, err = service.ReadExecutionFile(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	require.Empty(t, file.Content)
}
