package app_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestProfileStartupRetainsExactRevisionAndLegacyHash(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backend.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := testService(store, newFakeProvider())
	req := app.SaveConfigurationProfileRequest{Context: effect(model.OperatorPrincipal(), "save"), ID: "profile", RevisionID: "one", Name: "Profile", Desired: model.DesiredConfiguration{Harness: "fake", Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}}
	old, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	payload, err := json.Marshal(req.Desired)
	require.NoError(t, err)
	hash := sha256.Sum256(payload)
	require.Equal(t, hex.EncodeToString(hash[:]), old.Revision.Ref.ContentHash)
	req.Startup = &model.ProfileStartup{}
	replay, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	require.Equal(t, old, replay)
	req.Context.RequestID = "startup"
	req.RevisionID = "two"
	req.ExpectedRevision = 1
	req.Startup = &model.ProfileStartup{AgentName: "Writer", Context: "Review context", InitialMessage: "Explain first"}
	saved, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	require.NotEqual(t, old.Revision.Ref.ContentHash, saved.Revision.Ref.ContentHash)
	agent, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "writer", Name: "Writer", ConfigurationProfile: &saved.Revision.Ref})
	require.NoError(t, err)
	req.Startup.InitialMessage = "changed"
	_, err = service.SaveConfigurationProfile(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	req.Context.RequestID = "later"
	req.RevisionID = "three"
	req.ExpectedRevision = 2
	later, err := service.SaveConfigurationProfile(ctx, req)
	require.NoError(t, err)
	require.NotEqual(t, saved.Revision.Ref.ContentHash, later.Revision.Ref.ContentHash)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	service = testService(store, newFakeProvider())
	selected, err := service.GetConfigurationProfile(ctx, model.OperatorPrincipal(), *agent.Agent.ConfigurationProfile)
	require.NoError(t, err)
	require.Equal(t, "Explain first", selected.Revision.Startup.InitialMessage)
	for i, startup := range []model.ProfileStartup{{InitialMessage: strings.Repeat("x", 32769)}, {AgentName: strings.Repeat("x", 257)}, {AgentName: "bad\nname"}, {AgentName: "   "}, {Context: "bad\x00"}, {InitialMessage: string([]byte{0xff})}, {Context: strings.Repeat("x", 32767), InitialMessage: "y"}} {
		invalid := req
		invalid.Startup = &startup
		invalid.Context.RequestID = model.RequestID("invalid-" + string(rune('a'+i)))
		_, err = service.SaveConfigurationProfile(ctx, invalid)
		require.ErrorIs(t, err, app.ErrInvalid)
	}
}
