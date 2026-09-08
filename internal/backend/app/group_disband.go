package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
	"math"
	"time"
)

type DisbandGroupRequest struct {
	Context          RequestContext
	ID               model.GroupID
	ExpectedRevision model.Revision
}
type DisbandGroupResult struct {
	Group            model.Group
	DisbandedAt      time.Time
	ArchivedRules    []model.AutomationRuleID
	DetachedChildren []model.GroupID
}
type GroupDisbandAPI interface {
	DisbandGroup(context.Context, DisbandGroupRequest) (DisbandGroupResult, error)
}
type GroupDisbandStore interface {
	DisbandGroup(context.Context, DisbandGroupRequest, time.Time) (DisbandGroupResult, error)
}

func (s *Service) DisbandGroup(ctx context.Context, in DisbandGroupRequest) (DisbandGroupResult, error) {
	if err := validateEffectContext(in.Context); err != nil {
		return DisbandGroupResult{}, err
	}
	if in.Context.RequestID.Validate() != nil || in.ID.Validate() != nil || in.ExpectedRevision == 0 || in.ExpectedRevision >= math.MaxInt64 {
		return DisbandGroupResult{}, ErrInvalid
	}
	store, ok := s.store.(GroupDisbandStore)
	if !ok {
		return DisbandGroupResult{}, ErrUnsupported
	}
	return store.DisbandGroup(ctx, in, s.now().UTC())
}
