package app

import (
	"context"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type boundPrimaryObservationSink struct {
	service     *Service
	executionID model.ExecutionID
	attempt     model.AttemptGeneration
	provider    string
}

func (s *Service) primaryObservationSink(executionID model.ExecutionID, attempt model.AttemptGeneration, provider string) ports.PrimaryObservationSink {
	return &boundPrimaryObservationSink{service: s, executionID: executionID, attempt: attempt, provider: provider}
}

func (sink *boundPrimaryObservationSink) ObservePrimaryContext(ctx context.Context, evidence ports.PrimaryContextEvidence) error {
	if evidence.ExecutionID != sink.executionID || evidence.Attempt != sink.attempt || evidence.Provider != sink.provider {
		return fail(ErrUnauthorized, "observation does not match its bound provider attempt")
	}
	var resetConversation model.ConversationID
	if evidence.Disposition == ports.PrimaryContextReset {
		resetConversation = model.ConversationID(randomID("con_"))
	}
	_, err := sink.service.store.AdmitPrimaryContext(ctx, PrimaryContextAdmission{Evidence: evidence, ResetConversationID: resetConversation, At: sink.service.now().UTC()})
	return err
}
