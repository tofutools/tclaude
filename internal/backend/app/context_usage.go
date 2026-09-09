package app

import (
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func (s *Service) projectExecutionContext(execution model.Execution) model.Execution {
	execution.ContextUsage = nil
	if s.providers == nil {
		return execution
	}
	provider, ok := s.providers.Provider(execution.Spec.Harness)
	if !ok {
		return execution
	}
	if projector, ok := provider.(ports.ContextUsageProjector); ok {
		execution.ContextUsage = projector.ProjectContextUsage(execution)
	}
	return execution
}
