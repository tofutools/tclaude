package transport

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestOrchestrationAttributionExcludesAuthenticationGeneration(t *testing.T) {
	actor := model.ExecutionPrincipal("execution", "agent", 987654)
	for _, value := range []any{
		app.DefinitionResult{Revision: model.DefinitionRevision{Author: actor}},
		app.ProgramProfileResult{Revision: model.ProgramProfileRevision{Author: actor}},
		app.AutomationRuleResult{Revision: model.AutomationRuleRevision{Author: actor}},
		app.DecisionResult{Submission: &model.DecisionSubmission{Actor: actor}},
		app.OccurrenceResult{Occurrence: model.AutomationOccurrence{Requester: actor}},
	} {
		data, err := json.Marshal(projectOrchestration(value))
		require.NoError(t, err)
		require.Contains(t, string(data), `"execution_id":"execution"`)
		require.NotContains(t, string(data), "987654")
		require.NotContains(t, string(data), `"Generation"`)
		require.NotContains(t, string(data), `"Authority"`)
	}
}
