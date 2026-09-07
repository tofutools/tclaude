package app_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestProcessNotesSurviveStageCompilationAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "notes.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	graph := stagedHumanGraph()
	graph.Nodes[0].Description = "Task purpose"
	graph.Nodes[0].Doc = "<script>literal documentation</script>\nSecond line"
	graph.Nodes[0].Stages.Plan.Description = "Plan purpose"
	graph.Nodes[0].Stages.Plan.Doc = "Plan documentation"
	_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "notes"}, ID: "notes", Start: model.WorkStart{InlineGraph: &graph, Deadline: time.Now().Add(time.Hour)}})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	service = app.New(store, providers.NewRegistry())
	run, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "notes"})
	require.NoError(t, err)
	group := run.Run.Graph.TaskGroups[0]
	for _, node := range run.Run.Graph.Nodes {
		switch node.ID {
		case group.ID, group.Work:
			require.Equal(t, graph.Nodes[0].Description, node.Description)
			require.Equal(t, graph.Nodes[0].Doc, node.Doc)
		case group.Plan:
			require.Equal(t, "Plan purpose", node.Description)
			require.Equal(t, "Plan documentation", node.Doc)
			require.Equal(t, "Perform this stage", node.Performer.Human.Prompt, "notes must not replace performer instructions")
		}
	}
}

func TestProcessNotesRejectInvalidNodeAndStageText(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "notes.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	service := app.New(store, providers.NewRegistry())
	for _, stage := range []bool{false, true} {
		for _, text := range []string{strings.Repeat("x", (64<<10)+1), "nul\x00note", string([]byte{0xff})} {
			graph := stagedHumanGraph()
			if stage {
				graph.Nodes[0].Stages.Plan.Doc = text
			} else {
				graph.Nodes[0].Doc = text
			}
			_, err = service.StartProcess(context.Background(), app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "invalid"}, ID: "invalid", Start: model.WorkStart{InlineGraph: &graph, Deadline: time.Now().Add(time.Hour)}})
			require.ErrorIs(t, err, app.ErrInvalid)
		}
	}
}
