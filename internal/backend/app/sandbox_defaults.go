package app

import (
	"context"
	"errors"
	"math"

	"github.com/tofutools/tclaude/internal/backend/model"
)

type SaveSandboxDefaultsRequest struct {
	Context          RequestContext
	Global           model.SandboxProfileID
	Groups           map[model.GroupID]model.SandboxProfileID
	ExpectedRevision model.Revision
}

type SandboxDefaultsStore interface {
	SandboxGroupDisbanded(context.Context, model.GroupID) (bool, error)
	SandboxGroupsForAgent(context.Context, model.AgentID) ([]model.GroupID, error)
	SandboxDefaults(context.Context) (model.SandboxDefaults, error)
	SaveSandboxDefaults(context.Context, SaveSandboxDefaultsRequest) (model.SandboxDefaults, error)
}

type SandboxDefaultsAPI interface {
	GetSandboxDefaults(context.Context, model.Principal) (model.SandboxDefaults, error)
	SaveSandboxDefaults(context.Context, SaveSandboxDefaultsRequest) (model.SandboxDefaults, error)
}

func ValidateSandboxDefaults(in SaveSandboxDefaultsRequest) error {
	if err := validateEffectContext(in.Context); err != nil {
		return err
	}
	if err := requireOperator(in.Context.Principal); err != nil {
		return err
	}
	if in.ExpectedRevision >= math.MaxInt64 || len(in.Groups) > 4096 {
		return ErrInvalid
	}
	if in.Global != "" && in.Global.Validate() != nil {
		return ErrInvalid
	}
	for group, profile := range in.Groups {
		if group.Validate() != nil || profile.Validate() != nil {
			return ErrInvalid
		}
	}
	return nil
}

func (s *Service) GetSandboxDefaults(ctx context.Context, principal model.Principal) (model.SandboxDefaults, error) {
	if err := requireOperator(principal); err != nil {
		return model.SandboxDefaults{}, err
	}
	store, ok := s.store.(SandboxDefaultsStore)
	if !ok {
		return model.SandboxDefaults{}, ErrUnsupported
	}
	return store.SandboxDefaults(ctx)
}
func (s *Service) SaveSandboxDefaults(ctx context.Context, in SaveSandboxDefaultsRequest) (model.SandboxDefaults, error) {
	if err := ValidateSandboxDefaults(in); err != nil {
		return model.SandboxDefaults{}, err
	}
	store, ok := s.store.(SandboxDefaultsStore)
	if !ok {
		return model.SandboxDefaults{}, ErrUnsupported
	}
	return store.SaveSandboxDefaults(ctx, in)
}

// launchSandboxSelection re-reads mutable assignments for a fresh launch.
// Omission is a durable explicit choice, unlike an empty default registry.
func (s *Service) launchSandboxSelection(ctx context.Context, selected *model.SandboxSelection, agent model.Agent) (*model.SandboxSelection, error) {
	if err := model.ValidateSandboxSelection(selected); err != nil {
		return nil, fail(ErrInvalid, "%v", err)
	}
	if selected != nil && selected.OmitProfiles {
		return nil, nil
	}
	store, ok := s.store.(SandboxDefaultsStore)
	if !ok {
		return model.CloneSandboxSelection(selected), nil
	}
	defaults, err := store.SandboxDefaults(ctx)
	if err != nil {
		return nil, err
	}
	if agent.ID != "" && (selected == nil || selected.GroupID == "") {
		group := model.GroupID("")
		if agent.PrimaryExecutionID != "" {
			previous, err := s.store.Execution(ctx, agent.PrimaryExecutionID)
			if err != nil {
				return nil, err
			}
			if previous.Spec.HostSandbox != nil {
				group = previous.Spec.HostSandbox.GroupID
			}
		}
		if group == "" {
			groups, err := store.SandboxGroupsForAgent(ctx, agent.ID)
			if err != nil {
				return nil, err
			}
			if len(groups) > 0 {
				profile := defaults.Groups[groups[0]]
				for _, id := range groups[1:] {
					if defaults.Groups[id] != profile {
						return nil, fail(ErrConflict, "choose one sandbox launch group or give this agent's groups the same sandbox profile")
					}
				}
				if len(groups) == 1 || profile != "" {
					group = groups[0]
				}
			}
		}
		if group != "" {
			selected = model.SandboxInGroup(selected, group)
		}
	}
	if selected != nil && selected.GroupID != "" {
		if _, err := s.store.Group(ctx, selected.GroupID); err != nil {
			if !errors.Is(err, ErrNotFound) {
				return nil, err
			}
			removed, lookupErr := store.SandboxGroupDisbanded(ctx, selected.GroupID)
			if lookupErr != nil {
				return nil, lookupErr
			}
			if !removed {
				return nil, err
			}
			selected = model.CloneSandboxSelection(selected)
			selected.GroupID = ""
		}
	}
	return defaults.Resolve(selected), nil
}
