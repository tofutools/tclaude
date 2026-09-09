package migration

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportResolvedAgentApprovalModelAndEffortSurviveReopen(t *testing.T) {
	for _, tc := range []struct{ harness, approval string }{{"claude", "acceptEdits"}, {"codex", "on-request"}, {"opencode", "ask"}, {"copilot", "allow-tools"}} {
		t.Run(tc.harness, func(t *testing.T) {
			bundle := buildFixture(t, fixtureOptions{})
			birth, _ := json.Marshal(map[string]any{"harness": tc.harness, "model": "birth-model", "effort": "low", "approval": "automatic", "sandbox": "unconfined", "cwd": "/tmp"})
			resolved, _ := json.Marshal(map[string]any{"version": 1, "model_id": "selected-model", "effort": "high", "approval_policy": tc.approval})
			statement := "UPDATE agents SET initial_spawn_config='" + strings.ReplaceAll(string(birth), "'", "''") + "',relaunch_profile='" + strings.ReplaceAll(string(resolved), "'", "''") + "';"
			alterFixture(t, bundle, statement)
			path := filepath.Join(t.TempDir(), "target.db")
			_, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: path})
			require.NoError(t, err)
			store, err := backendsqlite.Open(path)
			require.NoError(t, err)
			defer store.Close()
			service := app.New(store, providers.NewRegistry())
			snapshot, err := service.Snapshot(context.Background(), app.SnapshotRequest{Principal: model.OperatorPrincipal()})
			require.NoError(t, err)
			require.Len(t, snapshot.Agents, 1)
			desired := snapshot.Agents[0].Desired
			require.Equal(t, "selected-model", desired.Model)
			require.Equal(t, "high", desired.Effort)
			require.Equal(t, model.ApprovalMode(tc.approval), desired.Approval)
			require.Empty(t, snapshot.Executions)
			repeated, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: path})
			require.NoError(t, err)
			require.True(t, repeated.Repeated)
		})
	}
}
