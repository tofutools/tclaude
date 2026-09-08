package transport

import (
	"bytes"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// Authored definitions remain visible while execution authentication generations
// and internal principal authority are replaced by public actor attribution.
type definitionRevisionView struct {
	model.DefinitionRevision
	Author            workActor
	Parameters        []parameterDeclarationView
	ProcessDurationNS map[string]string
}
type parameterDeclarationView struct {
	model.ParameterDeclaration
	DefaultJSON string
}

func projectParameters(parameters []model.ParameterDeclaration) []parameterDeclarationView {
	result := make([]parameterDeclarationView, 0, len(parameters))
	for _, p := range parameters {
		raw := p.Default
		// Application semantics treat missing and JSON null as no default.
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			raw = nil
		}
		result = append(result, parameterDeclarationView{p, string(raw)})
	}
	return result
}

type programRevisionView struct {
	model.ProgramProfileRevision
	Author workActor
}
type ruleRevisionView struct {
	model.AutomationRuleRevision
	Author workActor
}
type decisionSubmissionView struct {
	model.DecisionSubmission
	Actor workActor
}
type occurrenceView struct {
	model.AutomationOccurrence
	Requester workActor
}

func projectOrchestration(value any) any {
	switch v := value.(type) {
	case app.DefinitionResult:
		return struct {
			Definition model.Definition
			Revision   definitionRevisionView
		}{v.Definition, definitionRevisionView{v.Revision, projectWorkActor(v.Revision.Author), projectParameters(v.Revision.Parameters), processDurationProjection(v.Revision.Process)}}
	case app.ProgramProfileResult:
		return struct {
			Profile  model.ProgramProfile
			Revision programRevisionView
		}{v.Profile, programRevisionView{v.Revision, projectWorkActor(v.Revision.Author)}}
	case app.AutomationRuleResult:
		return struct {
			Rule     model.AutomationRule
			Revision ruleRevisionView
		}{v.Rule, ruleRevisionView{v.Revision, projectWorkActor(v.Revision.Author)}}
	case app.DecisionResult:
		var submission *decisionSubmissionView
		if v.Submission != nil {
			submission = &decisionSubmissionView{*v.Submission, projectWorkActor(v.Submission.Actor)}
		}
		return struct {
			Window     model.DecisionWindow
			Submission *decisionSubmissionView
		}{v.Window, submission}
	case []app.DecisionResult:
		result := make([]any, 0, len(v))
		for _, item := range v {
			result = append(result, projectOrchestration(item))
		}
		return result
	case app.OccurrenceResult:
		return struct{ Occurrence occurrenceView }{occurrenceView{v.Occurrence, projectWorkActor(v.Occurrence.Requester)}}
	case []app.OccurrenceResult:
		result := make([]any, 0, len(v))
		for _, item := range v {
			result = append(result, projectOrchestration(item))
		}
		return result
	default:
		return value
	}
}
