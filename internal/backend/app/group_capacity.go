package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
	"math"
	"time"
)

type SetGroupCapacityRequest struct {
	Principal        model.Principal
	ID               model.GroupID
	ExpectedRevision model.Revision
	MaxActiveMembers int64
}
type GroupCapacityAPI interface {
	SetGroupCapacity(context.Context, SetGroupCapacityRequest) (model.Group, error)
}
type GroupCapacityStore interface {
	SetGroupCapacity(context.Context, SetGroupCapacityRequest, time.Time) (model.Group, error)
}

func (s *Service) SetGroupCapacity(ctx context.Context, in SetGroupCapacityRequest) (model.Group, error) {
	if err := requireOperator(in.Principal); err != nil {
		return model.Group{}, err
	}
	if in.ID.Validate() != nil || in.ExpectedRevision == 0 || in.ExpectedRevision >= math.MaxInt64 || !model.ValidGroupCapacity(in.MaxActiveMembers) {
		return model.Group{}, ErrInvalid
	}
	store, ok := s.store.(GroupCapacityStore)
	if !ok {
		return model.Group{}, ErrUnsupported
	}
	return store.SetGroupCapacity(ctx, in, s.now().UTC())
}

// Reject already-full reinforcement before preparing its external workspace
// resources. Admission still rechecks capacity in its own write transaction.
func (s *Service) requireAvailableGroupCapacity(ctx context.Context, group model.Group, additional int) error {
	if group.MaxActiveMembers == 0 {
		return nil
	}
	active := int64(additional)
	for _, id := range group.Members {
		agent, err := s.store.Agent(ctx, id)
		if err != nil {
			return err
		}
		if agent.Lifecycle == model.AgentActive {
			active++
		}
	}
	if active > group.MaxActiveMembers {
		return ErrConflict
	}
	return nil
}
