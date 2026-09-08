package app_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestProcessContactAuthoringPreservesSlotsAndRefusesExecution(t *testing.T) {
	for _, stage := range []bool{false, true} {
		name := "task"
		if stage {
			name = "plan"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "contacts.sqlite")
			store, err := sqlite.Open(path)
			require.NoError(t, err)
			service := app.New(store, providers.NewRegistry())
			graph := stagedHumanGraph()
			contact := &model.ContactSchedule{Cadence: "30m", Budget: 5, EscalationTarget: "human:operator"}
			if stage {
				graph.Nodes[0].Stages.Plan.Performer.Contact = contact
			} else {
				graph.Nodes[0].Performer.Contact = contact
			}
			draft := app.DefinitionDraft{ID: "contact", RevisionID: "contact_v1", Name: "Contact authoring", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "kind: process", Process: &model.ProcessDefinition{Graph: graph}}
			saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: draft})
			require.NoError(t, err)
			require.NoError(t, store.Close())
			store, err = sqlite.Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			service = app.New(store, providers.NewRegistry())
			read, err := service.GetDefinition(ctx, app.GetDefinitionRequest{Principal: model.OperatorPrincipal(), DefinitionID: "contact"})
			require.NoError(t, err)
			if stage {
				require.Equal(t, contact, read.Revision.Process.Graph.Nodes[0].Stages.Plan.Performer.Contact)
			} else {
				require.Equal(t, contact, read.Revision.Process.Graph.Nodes[0].Performer.Contact)
			}
			ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
			_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "run"}, ID: "run", Start: model.WorkStart{Definition: &ref, Deadline: time.Now().Add(time.Hour)}})
			require.ErrorIs(t, err, app.ErrUnsupported)
			_, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
			require.ErrorIs(t, err, app.ErrNotFound)
			for _, invalid := range []model.ContactSchedule{{Budget: 1, EscalationTarget: "operator"}, {Cadence: "1m", EscalationTarget: "operator"}, {Cadence: "1m", Budget: 1}, {Cadence: "-1m", Budget: 1, EscalationTarget: "operator"}} {
				*contact = invalid
				_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
				require.ErrorIs(t, err, app.ErrInvalid)
			}
		})
	}
}
