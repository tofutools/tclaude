package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

type SetGroupConfigurationRequest struct {
	Environment      model.Environment
	Principal        model.Principal
	GroupID          model.GroupID
	Profile          *model.ConfigurationProfileRef
	ExpectedRevision model.Revision
}
type CreateGroupMemberRequest struct {
	Labels                  *model.AgentDisplayLabels
	Environment             model.Environment
	Context                 RequestContext
	GroupID                 model.GroupID
	ID                      model.AgentID
	Name                    string
	ExpectedGroupRevision   model.Revision
	ExpectedDefaultRevision model.Revision
}
type GroupMemberResult struct {
	Agent    model.Agent
	Group    model.Group
	Repeated bool
}
type GroupConfigurationAPI interface {
	GetGroupConfiguration(context.Context, model.Principal, model.GroupID) (model.GroupConfiguration, error)
	SetGroupConfiguration(context.Context, SetGroupConfigurationRequest) (model.GroupConfiguration, error)
	CreateGroupMember(context.Context, CreateGroupMemberRequest) (GroupMemberResult, error)
}
type GroupConfigurationStore interface {
	GroupConfiguration(context.Context, model.GroupID) (model.GroupConfiguration, error)
	SetGroupConfiguration(context.Context, SetGroupConfigurationRequest, time.Time) (model.GroupConfiguration, error)
	FindGroupMemberAdmission(context.Context, CreateGroupMemberRequest) (GroupMemberResult, bool, error)
	AdmitGroupMember(context.Context, CreateGroupMemberRequest, model.Agent, time.Time) (GroupMemberResult, error)
}

func (s *Service) GetGroupConfiguration(ctx context.Context, p model.Principal, id model.GroupID) (model.GroupConfiguration, error) {
	if err := requireOperator(p); err != nil {
		return model.GroupConfiguration{}, err
	}
	if id.Validate() != nil {
		return model.GroupConfiguration{}, ErrInvalid
	}
	store, ok := s.store.(GroupConfigurationStore)
	if !ok {
		return model.GroupConfiguration{}, ErrUnsupported
	}
	return store.GroupConfiguration(ctx, id)
}
func (s *Service) SetGroupConfiguration(ctx context.Context, in SetGroupConfigurationRequest) (model.GroupConfiguration, error) {
	if err := requireOperator(in.Principal); err != nil {
		return model.GroupConfiguration{}, err
	}
	if in.Environment.Validate() != nil || in.GroupID.Validate() != nil || in.ExpectedRevision >= math.MaxInt64 {
		return model.GroupConfiguration{}, ErrInvalid
	}
	if in.Profile != nil {
		desired, _, err := s.resolveConfigurationSelection(ctx, model.DesiredConfiguration{}, in.Profile)
		if err != nil {
			return model.GroupConfiguration{}, err
		}
		if err = validateDesired(desired); err != nil {
			return model.GroupConfiguration{}, err
		}
	}
	store, ok := s.store.(GroupConfigurationStore)
	if !ok {
		return model.GroupConfiguration{}, ErrUnsupported
	}
	return store.SetGroupConfiguration(ctx, in, s.now().UTC())
}
func (s *Service) CreateGroupMember(ctx context.Context, in CreateGroupMemberRequest) (GroupMemberResult, error) {
	if err := requireOperator(in.Context.Principal); err != nil {
		return GroupMemberResult{}, err
	}
	if in.Labels != nil {
		if err := (model.AgentLabels{Role: in.Labels.Role, Description: in.Labels.Description}).Validate(); err != nil {
			return GroupMemberResult{}, fail(ErrInvalid, "%v", err)
		}
	}
	if in.Environment.Validate() != nil || in.Context.RequestID.Validate() != nil || in.GroupID.Validate() != nil || in.ID.Validate() != nil || strings.TrimSpace(in.Name) == "" || len(in.Name) > 1024 || !utf8.ValidString(in.Name) || in.ExpectedGroupRevision == 0 || in.ExpectedGroupRevision >= math.MaxInt64 || in.ExpectedDefaultRevision == 0 || in.ExpectedDefaultRevision >= math.MaxInt64 {
		return GroupMemberResult{}, ErrInvalid
	}
	store, ok := s.store.(GroupConfigurationStore)
	if !ok {
		return GroupMemberResult{}, ErrUnsupported
	}
	if prior, found, err := store.FindGroupMemberAdmission(ctx, in); found || err != nil {
		return prior, err
	}
	defaults, err := store.GroupConfiguration(ctx, in.GroupID)
	if err != nil {
		return GroupMemberResult{}, err
	}
	if defaults.Revision != in.ExpectedDefaultRevision || defaults.Profile == nil {
		return GroupMemberResult{}, ErrConflict
	}
	desired, ref, err := s.resolveConfigurationSelection(ctx, model.DesiredConfiguration{}, defaults.Profile)
	if err != nil {
		return GroupMemberResult{}, err
	}
	desired.Environment, err = model.MergeEnvironment(defaults.Environment, desired.Environment, in.Environment)
	if err != nil {
		return GroupMemberResult{}, fail(ErrInvalid, "%v", err)
	}
	if err = validateDesired(desired); err != nil {
		return GroupMemberResult{}, err
	}
	desired.HostSandbox = model.SandboxInGroup(desired.HostSandbox, in.GroupID)
	labels, err := s.configurationDisplayLabels(ctx, ref, nil)
	if err != nil {
		return GroupMemberResult{}, err
	}
	memberLabels := model.AgentDisplayLabels{Role: labels.Role, Description: labels.Description}
	if in.Labels != nil {
		memberLabels = *in.Labels
	}
	labels = model.AgentLabels{Groups: map[model.GroupID]model.AgentDisplayLabels{in.GroupID: memberLabels}}
	now := s.now().UTC()
	agent := model.Agent{Labels: labels, ID: in.ID, Name: in.Name, Lifecycle: model.AgentActive, Notifications: model.AgentNotificationPreferences{DirectMessage: model.NotificationIfAvailable}, Desired: desired, ConfigurationProfile: ref, Revision: 1, CreatedAt: now, UpdatedAt: now}
	return store.AdmitGroupMember(ctx, in, agent, now)
}
