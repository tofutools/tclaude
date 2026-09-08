package app_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	db "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestRecoveryPassesSeparatelyAdmittedPrimaryWithOriginalProviderEvidence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := db.Open(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	provider := newFakeProvider()
	service := testService(store, provider)
	op := model.OperatorPrincipal()
	agent := createAgent(t, ctx, service, op, "worker")
	launch, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(op, "launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)
	current, err := store.Execution(ctx, launch.Execution.ID)
	require.NoError(t, err)
	require.NotNil(t, current.NativeConversation)
	original := current.Evidence
	prior := &model.NativeBinding{Namespace: current.NativeConversation.Namespace, Reference: current.NativeConversation.Reference}
	next := &model.NativeBinding{Namespace: prior.Namespace, Reference: "separately_admitted_primary"}
	admitted, err := store.AdmitPrimaryContext(ctx, app.PrimaryContextAdmission{Evidence: ports.PrimaryContextEvidence{ExecutionID: current.ID, Attempt: current.Attempt, Provider: "fake", PrimaryCorrelation: "primary_recovery", Disposition: ports.PrimaryContextContinuity, PriorBinding: prior, NextBinding: next, PriorProviderOrder: current.ContextOrder, ProviderOrder: "next_order", ObservedAt: time.Now().UTC()}, At: time.Now().UTC()})
	require.NoError(t, err)
	require.Equal(t, original, admitted.Evidence, "primary admission does not regenerate release evidence")
	provider.runtime.native.Reference = next.Reference
	require.NoError(t, store.Close())
	store, err = db.Open(path)
	require.NoError(t, err)
	service = testService(store, provider)
	report, err := service.Recover(ctx, app.RecoverRequest{Principal: op})
	require.NoError(t, err)
	require.Contains(t, report.Controlled, current.ID)
	require.Equal(t, original, provider.lastRecovery.Evidence)
	require.Equal(t, &ports.PrimaryContextRecovery{Binding: *next, Readiness: admitted.ContextReadiness, ProviderOrder: "next_order"}, provider.lastRecovery.PrimaryContext)
}
