package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestHumanQuestionAcceptsEitherFieldAndRejectsInvalidText(t *testing.T) {
	for name, human := range map[string]model.HumanPerformer{
		"question": {Ask: "Question?"}, "context": {Prompt: "Context"}, "both": {Ask: "Question?", Prompt: "Context"},
		"empty": {}, "nul": {Ask: "bad\x00", Prompt: "Context"}, "utf8": {Ask: string([]byte{0xff}), Prompt: "Context"}, "oversize": {Ask: strings.Repeat("x", (1<<20)+1), Prompt: "Context"},
	} {
		t.Run(name, func(t *testing.T) {
			_, service, _ := regressionService(t)
			graph := stagedHumanGraph()
			graph.Nodes[0].Stages = nil
			human.Operator = true
			graph.Nodes[0].Performer.Human = &human
			_, err := service.ValidateDefinition(context.Background(), app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: app.DefinitionDraft{ID: "question", Name: "Question", Source: "process", SchemaVersion: 1, Kind: model.DefinitionProcess, Process: &model.ProcessDefinition{Graph: graph}}})
			if name == "question" || name == "context" || name == "both" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, app.ErrInvalid)
			}
		})
	}
}
