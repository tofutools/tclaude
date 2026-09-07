package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
	"math"
	"time"
)

type SetGroupDetailsRequest struct {
	Principal        model.Principal
	ID               model.GroupID
	ExpectedRevision model.Revision
	Details          model.GroupDetails
}
type GroupDetailsAPI interface {
	SetGroupDetails(context.Context, SetGroupDetailsRequest) (model.Group, error)
}
type GroupDetailsStore interface {
	SetGroupDetails(context.Context, SetGroupDetailsRequest, time.Time) (model.Group, error)
}

func (s *Service) SetGroupDetails(ctx context.Context, in SetGroupDetailsRequest) (model.Group, error) {
	if err := requireOperator(in.Principal); err != nil {
		return model.Group{}, err
	}
	if in.ID.Validate() != nil || in.ExpectedRevision == 0 || in.ExpectedRevision >= math.MaxInt64 || model.ValidateGroupDetails(in.Details) != nil {
		return model.Group{}, ErrInvalid
	}
	store, ok := s.store.(GroupDetailsStore)
	if !ok {
		return model.Group{}, ErrUnsupported
	}
	return store.SetGroupDetails(ctx, in, s.now().UTC())
}
