//go:build linux || darwin

package app_test

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
	"time"
)

func TestPerformerTimeoutRealProgramKeepsProfileAndRunBounds(t *testing.T) {
	// Leave real host preparation enough time on a contended race runner; the
	// assertions still prove both sides of the profile-versus-node minimum.
	const nodeTimeout = 5 * time.Second
	for _, profileTimeout := range []time.Duration{30 * time.Second, 3 * time.Second} {
		t.Run(profileTimeout.String(), func(t *testing.T) {
			ctx := context.Background()
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
			require.NoError(t, err)
			defer store.Close()
			service := app.New(store, providers.NewRegistry()).WithProgramHost(&host.ProgramProcessHost{PrivateRoot: filepath.Join(t.TempDir(), "host")})
			now := time.Now().UTC()
			cwd := t.TempDir()
			require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: cwd, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
			profile, err := service.SaveProgramProfile(ctx, app.SaveProgramProfileRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "profile"}, ID: "sleep", RevisionID: "v1", Name: "sleep", Executable: "/bin/sh", ArgumentPrefix: []string{"-c", "sleep 30"}, Sandbox: model.SandboxUnconfined, Timeout: profileTimeout, OutputLimitBytes: 1024, EffectAuthority: []model.ProgramEffectRequirement{{Action: model.ActionExecuteProgram, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace}}}})
			require.NoError(t, err)
			graph := programGraph(profile)
			graph.Nodes[0].Performer.Timeout = nodeTimeout.String()
			started, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Scope: model.WorkScope{WorkspaceID: "workspace"}, AuthorizedProgramProfiles: []model.ProgramProfileRef{graph.Nodes[0].Performer.Program.Profile}, Deadline: time.Now().Add(time.Minute)}})
			require.NoError(t, err)
			require.WithinDuration(t, started.Run.NodeAttempts[0].ReadyAt.Add(min(nodeTimeout, profileTimeout)), started.Run.NodeAttempts[0].Deadline, time.Millisecond)
			_, err = service.ReconcilePendingWork(ctx)
			require.NoError(t, err)
			var result app.WorkRunResult
			require.Eventually(t, func() bool {
				_, err = service.ReconcilePendingWork(ctx)
				if err != nil {
					return false
				}
				result, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
				return err == nil && result.Run.State == model.WorkRunFailed
			}, 10*time.Second, 20*time.Millisecond)
			attempt := nodeAttempt(t, result.Run, "program")
			require.NotEmpty(t, attempt.Ref.IssuanceID)
			execution, err := store.Execution(ctx, attempt.ExecutionID)
			require.NoError(t, err)
			require.Contains(t, []model.ExecutionState{model.ExecutionExited, model.ExecutionFailed}, execution.State)
			uses, err := store.ActiveWorkspaceUses(ctx, "workspace")
			require.NoError(t, err)
			require.Empty(t, uses)
			var evidence struct {
				Deadline time.Time `json:"deadline"`
			}
			require.NoError(t, json.Unmarshal(execution.Evidence.Payload, &evidence))
			require.False(t, evidence.Deadline.After(attempt.Deadline))
			if profileTimeout < nodeTimeout {
				require.Equal(t, attempt.ReadyAt.Add(profileTimeout), evidence.Deadline, "saved profile timeout is pinned from activation readiness")
			}
			require.Equal(t, profileTimeout, profile.Revision.Timeout)
		})
	}
}
