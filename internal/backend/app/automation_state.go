package app

import (
	"context"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Service) SetAutomationEnabled(ctx context.Context, req SetAutomationEnabledRequest) (model.AutomationRule, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return model.AutomationRule{}, err
	}
	if err := req.ID.Validate(); err != nil {
		return model.AutomationRule{}, ErrInvalid
	}
	if req.ExpectedRevision == 0 {
		return model.AutomationRule{}, ErrInvalid
	}
	return s.store.SetAutomationEnabled(ctx, req, s.now().UTC())
}
