package app

import (
	"context"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

type ShellRequestStore interface {
	FindShellAdmission(context.Context, StartShellRequest, time.Time) (AdmissionResult, bool, error)
}

func (s *Service) shellEnvironment(ctx context.Context, in StartShellRequest) (model.Environment, error) {
	if err := in.Environment.Validate(); err != nil {
		return nil, fail(ErrInvalid, "%v", err)
	}
	if in.Group == nil {
		return in.Environment.Clone(), nil
	}
	if err := requireOperator(in.Context.Principal); err != nil {
		return nil, err
	}
	if in.Group.GroupID.Validate() != nil || in.Group.Revision == 0 {
		return nil, ErrInvalid
	}
	group, err := s.store.Group(ctx, in.Group.GroupID)
	if err != nil {
		return nil, err
	}
	if group.Revision != in.Group.Revision {
		return nil, ErrConflict
	}
	store, ok := s.store.(GroupConfigurationStore)
	if !ok {
		return nil, ErrUnsupported
	}
	config, err := store.GroupConfiguration(ctx, in.Group.GroupID)
	if err != nil {
		return nil, err
	}
	if config.Revision != in.Group.ConfigurationRevision {
		return nil, ErrConflict
	}
	out, err := model.MergeEnvironment(config.Environment, in.Environment)
	if err != nil {
		return nil, fail(ErrInvalid, "%v", err)
	}
	return out, nil
}
