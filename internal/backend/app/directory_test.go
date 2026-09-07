package app_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type directoryProbe struct{ called bool }

func (p *directoryProbe) ReadDirectory(_ context.Context, r ports.DirectoryReadRequest) (model.DirectoryListing, error) {
	p.called = true
	return model.DirectoryListing{Path: r.Path}, nil
}
func TestDirectoryBrowsingRequiresOperatorBeforeHostRead(t *testing.T) {
	p := &directoryProbe{}
	service := (&app.Service{}).WithDirectoryBrowser(p)
	req := app.BrowseDirectoryRequest{Principal: model.Principal{Kind: model.PrincipalExecution, ExecutionID: "execution"}, Path: "/explicit-path"}
	_, err := service.BrowseDirectory(context.Background(), req)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	require.False(t, p.called)
	req.Principal = model.OperatorPrincipal()
	req.Path = "relative"
	_, err = service.BrowseDirectory(context.Background(), req)
	require.ErrorIs(t, err, app.ErrInvalid)
	require.False(t, p.called)
	req.Path = "/explicit-path"
	out, err := service.BrowseDirectory(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, req.Path, out.Path)
	require.True(t, p.called)
}
