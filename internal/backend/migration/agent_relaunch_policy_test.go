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
			resolvedModel := "selected-model"
			wantModel := resolvedModel
			if tc.harness == "claude" {
				wantModel += "[1m]"
			}
			resolved, _ := json.Marshal(map[string]any{"version": 1, "model_id": resolvedModel, "context_window_size": 1000000, "effort": "high", "approval_policy": tc.approval})
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
			require.Equal(t, wantModel, desired.Model)
			require.Equal(t, "high", desired.Effort)
			require.Equal(t, model.ApprovalMode(tc.approval), desired.Approval)
			require.Empty(t, snapshot.Executions)
			repeated, err := ImportSnapshot(context.Background(), bundle, ImportOptions{DestinationPath: path})
			require.NoError(t, err)
			require.True(t, repeated.Repeated)
		})
	}
}

func TestResolvedModelContextWindowMatchesV1(t *testing.T) {
	for _, tc := range []struct{ name, harness, raw, want string }{
		{"extended", "claude", `{"model_id":"claude-opus-4-8","context_window_size":1000000}`, "claude-opus-4-8[1m]"},
		{"exactly once", "claude", `{"model_id":"claude-opus-4-8[1m]","context_window_size":1000000}`, "claude-opus-4-8[1m]"},
		{"ordinary", "claude", `{"model_id":"claude-opus-4-8[1m]","context_window_size":200000}`, "claude-opus-4-8"},
		{"missing model retains birth", "claude", `{"context_window_size":1000000}`, "birth-model"},
		{"empty model", "claude", `{"model_id":"","context_window_size":1000000}`, ""},
		{"foreign provider", "codex", `{"model_id":"gpt-5","context_window_size":1000000}`, "gpt-5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw map[string]any
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &raw))
			raw["version"] = 1
			encoded, err := json.Marshal(raw)
			require.NoError(t, err)
			desired, err := applyAgentRelaunchPolicy(map[string]any{"relaunch_profile": string(encoded)}, model.DesiredConfiguration{Harness: tc.harness, Model: "birth-model"})
			require.NoError(t, err)
			require.Equal(t, tc.want, desired.Model)
		})
	}
}
