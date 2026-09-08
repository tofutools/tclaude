package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
)

type SetDefinitionArchivedRequest struct {
	Context          RequestContext
	ID               model.DefinitionID
	ExpectedRevision model.Revision
	Archived         bool
}

func (s *Service) SetDefinitionArchived(ctx context.Context, req SetDefinitionArchivedRequest) (model.Definition, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return model.Definition{}, err
	}
	if req.ID.Validate() != nil || req.ExpectedRevision == 0 {
		return model.Definition{}, ErrInvalid
	}
	return s.store.SetDefinitionArchived(ctx, req, s.now().UTC())
}
