package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
	"math"
	"time"
)

type SetGroupParentRequest struct {
	Context          RequestContext
	ID               model.GroupID
	ParentGroupID    model.GroupID
	ExpectedRevision model.Revision
}
type GroupHierarchyAPI interface {
	SetGroupParent(context.Context, SetGroupParentRequest) (model.Group, error)
}
type GroupHierarchyStore interface {
	SetGroupParent(context.Context, SetGroupParentRequest, time.Time) (model.Group, error)
}

func (s *Service) SetGroupParent(ctx context.Context, in SetGroupParentRequest) (model.Group, error) {
	if err := requireOperator(in.Context.Principal); err != nil {
		return model.Group{}, err
	}
	if in.Context.RequestID.Validate() != nil || in.ID.Validate() != nil || (in.ParentGroupID != "" && in.ParentGroupID.Validate() != nil) || in.ExpectedRevision == 0 || in.ExpectedRevision >= math.MaxInt64 {
		return model.Group{}, ErrInvalid
	}
	store, ok := s.store.(GroupHierarchyStore)
	if !ok {
		return model.Group{}, ErrUnsupported
	}
	return store.SetGroupParent(ctx, in, s.now().UTC())
}
