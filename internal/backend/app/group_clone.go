package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

type CloneGroupRequest struct {
	Context                 RequestContext
	SourceID                model.GroupID
	ID                      model.GroupID
	Name                    string
	ExpectedGroupRevision   model.Revision
	ExpectedDefaultRevision model.Revision
	ExpectedMembers         map[model.AgentID]model.Revision
	CopyMembers             bool
	CopyDefault             bool
	MaxActiveMembers        int64
}
type CloneGroupResult struct {
	Group    model.Group
	Members  map[model.AgentID]model.AgentID
	Repeated bool
}
type GroupCloneAPI interface {
	CloneGroup(context.Context, CloneGroupRequest) (CloneGroupResult, error)
}
type GroupCloneStore interface {
	FindGroupClone(context.Context, CloneGroupRequest) (CloneGroupResult, bool, error)
	AdmitGroupClone(context.Context, CloneGroupRequest, []model.Agent, time.Time) (CloneGroupResult, error)
}

func (s *Service) CloneGroup(ctx context.Context, in CloneGroupRequest) (CloneGroupResult, error) {
	if err := requireOperator(in.Context.Principal); err != nil {
		return CloneGroupResult{}, err
	}
	if err := validateEffectContext(in.Context); err != nil {
		return CloneGroupResult{}, err
	}
	if in.SourceID.Validate() != nil || in.ID.Validate() != nil || in.SourceID == in.ID || strings.TrimSpace(in.Name) == "" || len(in.Name) > 1024 || !utf8.ValidString(in.Name) || strings.ContainsRune(in.Name, 0) || !model.ValidGroupCapacity(in.MaxActiveMembers) || in.ExpectedGroupRevision == 0 || in.ExpectedGroupRevision >= math.MaxInt64 || len(in.ExpectedMembers) > 1024 {
		return CloneGroupResult{}, ErrInvalid
	}
	if !in.CopyMembers && len(in.ExpectedMembers) != 0 || !in.CopyDefault && in.ExpectedDefaultRevision != 0 || in.CopyDefault && (in.ExpectedDefaultRevision == 0 || in.ExpectedDefaultRevision >= math.MaxInt64) {
		return CloneGroupResult{}, ErrInvalid
	}
	for id, revision := range in.ExpectedMembers {
		if id.Validate() != nil || revision == 0 || revision >= math.MaxInt64 {
			return CloneGroupResult{}, ErrInvalid
		}
	}
	store, ok := s.store.(GroupCloneStore)
	if !ok {
		return CloneGroupResult{}, ErrUnsupported
	}
	if prior, found, err := store.FindGroupClone(ctx, in); found || err != nil {
		return prior, err
	}
	source, err := s.store.Group(ctx, in.SourceID)
	if err != nil {
		return CloneGroupResult{}, err
	}
	if source.Revision != in.ExpectedGroupRevision {
		return CloneGroupResult{}, ErrConflict
	}
	now := s.now().UTC()
	var clones []model.Agent
	if in.CopyMembers {
		if len(source.Members) != len(in.ExpectedMembers) {
			return CloneGroupResult{}, ErrConflict
		}
		for _, id := range source.Members {
			agent, err := s.store.Agent(ctx, id)
			if err != nil {
				return CloneGroupResult{}, err
			}
			if agent.Revision != in.ExpectedMembers[id] {
				return CloneGroupResult{}, ErrConflict
			}
			if agent.Lifecycle != model.AgentActive {
				continue
			}
			if err = validateDesired(agent.Desired); err != nil {
				return CloneGroupResult{}, err
			}
			if err = validateAgentMetadata(agent.TaskReference, agent.Notifications); err != nil {
				return CloneGroupResult{}, err
			}
			if strings.TrimSpace(agent.Name) == "" {
				return CloneGroupResult{}, ErrInvalid
			}
			labels := agent.Labels.InGroup(source.ID)
			clone := model.Agent{ID: model.AgentID(deterministicOrchestrationID("agent_", string(in.ID)+":"+string(id))), Name: agent.Name, Labels: model.AgentLabels{Role: labels.Role, Description: labels.Description}, TaskReference: agent.TaskReference, CloneSourceAgentID: id, Lifecycle: model.AgentActive, Notifications: agent.Notifications, Desired: agent.Desired, ConfigurationProfile: agent.ConfigurationProfile, Revision: 1, CreatedAt: now, UpdatedAt: now}
			clone.Desired.HostSandbox = model.SandboxInGroup(clone.Desired.HostSandbox, in.ID)
			clones = append(clones, clone)
		}
	}
	if in.MaxActiveMembers > 0 && int64(len(clones)) > in.MaxActiveMembers {
		return CloneGroupResult{}, ErrConflict
	}
	return store.AdmitGroupClone(ctx, in, clones, now)
}
