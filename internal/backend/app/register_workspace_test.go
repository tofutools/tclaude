package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

type registrationDispositionHost struct {
	journeyWorkspaceHost
	disposition ports.EffectDisposition
	failure     error
	calls       int
}

func (h *registrationDispositionHost) InspectWorkspace(context.Context, model.Workspace) (ports.WorkspaceEffectResult, error) {
	h.calls++
	return ports.WorkspaceEffectResult{Disposition: h.disposition}, h.failure
}

func TestRegistrationPreservesUnknownDispositionAndExactRetry(t *testing.T) {
	for _, tc := range []struct {
		name        string
		disposition ports.EffectDisposition
		failure     error
		want        error
		state       model.WorkspaceState
	}{
		{"unknown", ports.EffectUnknown, nil, app.ErrUncertain, model.WorkspaceUncertain},
		{"unknown with diagnostic", ports.EffectUnknown, errors.New("inspection interrupted"), app.ErrUncertain, model.WorkspaceUncertain},
		{"missing disposition", "", errors.New("inspection interrupted"), app.ErrUncertain, model.WorkspaceUncertain},
		{"refused", ports.EffectRefused, nil, app.ErrUnavailable, model.WorkspacePending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "review.db"))
			require.NoError(t, err)
			defer store.Close()

			h := &registrationDispositionHost{disposition: tc.disposition, failure: tc.failure}
			service := app.New(store, providers.NewRegistry(newJourneyProvider())).WithWorkspaceHost(h)
			req := app.RegisterWorkspaceRequest{
				Context: request(model.OperatorPrincipal(), "register-unknown"),
				ID:      "external",
				Intent:  model.WorkspaceIntent{IntendedPath: t.TempDir()},
			}

			first, err := service.RegisterWorkspace(ctx, req)
			assert.ErrorIs(t, err, tc.want)
			assert.Equal(t, tc.state, first.Workspace.State)

			_, err = service.RegisterWorkspace(ctx, req)
			assert.ErrorIs(t, err, tc.want)
			require.Equal(t, 1, h.calls)
		})
	}
}
