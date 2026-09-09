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
type GroupMemberLaunch struct {
	InitialMessage string `json:"initial_message"`
}

type CreateGroupMemberRequest struct {
	Workspace               *model.WorkspaceSelection
	ProfileID               model.ConfigurationProfileID
	Launch                  *GroupMemberLaunch
	ConfigurationOverrides  *model.ConfigurationOptions
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
	Operation *OperationResult `json:",omitempty"`
	Agent     model.Agent
	Group     model.Group
	Repeated  bool
}
type GroupConfigurationAPI interface {
	GetGroupConfiguration(context.Context, model.Principal, model.GroupID) (model.GroupConfiguration, error)
	SetGroupConfiguration(context.Context, SetGroupConfigurationRequest) (model.GroupConfiguration, error)
	CreateGroupMember(context.Context, CreateGroupMemberRequest) (GroupMemberResult, error)
}
type GroupMemberAdmission struct {
	Request       CreateGroupMemberRequest
	Agent         model.Agent
	Sources       *model.TeamConfigurationSources
	Configuration ResolvedProfileConfiguration
	At            time.Time
}

type GroupConfigurationStore interface {
	GroupConfiguration(context.Context, model.GroupID) (model.GroupConfiguration, error)
	SetGroupConfiguration(context.Context, SetGroupConfigurationRequest, time.Time) (model.GroupConfiguration, error)
	FindGroupMemberAdmission(context.Context, CreateGroupMemberRequest, time.Time) (GroupMemberResult, bool, error)
	AdmitGroupMember(context.Context, GroupMemberAdmission) (GroupMemberResult, error)
}

func (s *Service) GetGroupConfiguration(ctx context.Context, p model.Principal, id model.GroupID) (model.GroupConfiguration, error) {
	if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: p, Action: model.ActionCreateGroupMember, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: id}}, s.now().UTC()); err != nil {
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
		if err := s.validateProfileDefaultSelection(ctx, *in.Profile, ""); err != nil {
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
	if err := validateEffectContext(in.Context); err != nil {
		return GroupMemberResult{}, err
	}
	if in.Labels != nil {
		if err := (model.AgentLabels{Role: in.Labels.Role, Description: in.Labels.Description}).Validate(); err != nil {
			return GroupMemberResult{}, fail(ErrInvalid, "%v", err)
		}
	}
	if in.Environment.Validate() != nil || in.Context.RequestID.Validate() != nil || in.GroupID.Validate() != nil || in.ID.Validate() != nil || strings.TrimSpace(in.Name) == "" || len(in.Name) > 1024 || !utf8.ValidString(in.Name) || in.ExpectedGroupRevision == 0 || in.ExpectedGroupRevision >= math.MaxInt64 || in.ExpectedDefaultRevision >= math.MaxInt64 {
		return GroupMemberResult{}, ErrInvalid
	}
	if in.Workspace != nil && (in.Workspace.WorkspaceID.Validate() != nil || in.Workspace.ExpectedRevision == 0 || in.Workspace.ExpectedRevision >= math.MaxInt64) {
		return GroupMemberResult{}, ErrInvalid
	}
	if in.ProfileID != "" && model.ValidateStableID("configuration profile", string(in.ProfileID)) != nil {
		return GroupMemberResult{}, ErrInvalid
	}
	store, ok := s.store.(GroupConfigurationStore)
	if !ok {
		return GroupMemberResult{}, ErrUnsupported
	}
	if prior, found, err := store.FindGroupMemberAdmission(ctx, in, s.now().UTC()); found || err != nil {
		if err == nil && in.Launch != nil {
			return s.launchGroupMember(ctx, store, in, prior.Agent, nil)
		}
		return prior, err
	}
	resolvedRequest, err := s.groupMemberWorkspaceRequest(ctx, in)
	if err != nil {
		return GroupMemberResult{}, err
	}
	admission, err := s.resolveGroupMember(ctx, store, resolvedRequest)
	admission.Request = in
	if err != nil {
		return GroupMemberResult{}, err
	}
	if in.Launch == nil {
		return store.AdmitGroupMember(ctx, admission)
	}
	return s.launchGroupMember(ctx, store, in, admission.Agent, &admission)
}

func (s *Service) launchGroupMember(ctx context.Context, store GroupConfigurationStore, in CreateGroupMemberRequest, agent model.Agent, admission *GroupMemberAdmission) (GroupMemberResult, error) {
	operation, err := s.launch(ctx, LaunchRequest{RequestContext: in.Context, InitialMessage: in.Launch.InitialMessage, Target: LaunchTarget{Agent: &AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}}, model.OperationLaunch, nil, launchOptions{groupMember: admission})
	if err != nil && operation.Operation.ID == "" {
		return GroupMemberResult{}, err
	}
	launchErr := err
	result, found, err := store.FindGroupMemberAdmission(ctx, in, s.now().UTC())
	if err != nil {
		return GroupMemberResult{}, err
	}
	if !found {
		return GroupMemberResult{}, ErrConflict
	}
	result.Agent, err = s.store.Agent(ctx, agent.ID)
	if err != nil {
		return GroupMemberResult{}, err
	}
	result.Operation = &operation
	result.Repeated = operation.Repeated
	return result, launchErr
}

func (s *Service) resolveGroupMember(ctx context.Context, store GroupConfigurationStore, in CreateGroupMemberRequest) (GroupMemberAdmission, error) {
	defaults, err := store.GroupConfiguration(ctx, in.GroupID)
	if err != nil {
		return GroupMemberAdmission{}, err
	}
	if defaults.Revision != in.ExpectedDefaultRevision {
		return GroupMemberAdmission{}, ErrConflict
	}
	if in.ProfileID != "" {
		return s.resolveSelectedGroupMember(ctx, in, defaults)
	}
	if defaults.Profile == nil {
		return s.resolveGroupMemberFallback(ctx, in, defaults)
	}
	current, err := s.currentConfigurationDefault(ctx, *defaults.Profile, "")
	if err != nil {
		return GroupMemberAdmission{}, err
	}
	profile, err := s.store.ConfigurationProfile(ctx, current.ProfileID, current.RevisionID)
	if err != nil {
		return GroupMemberAdmission{}, err
	}
	if profile.Profile.Archived || profile.Revision.Ref != *current {
		return GroupMemberAdmission{}, ErrConflict
	}
	if err := s.requireProfileCreation(ctx, in.Context.Principal, profile.Profile); err != nil {
		return GroupMemberAdmission{}, err
	}
	configuration, err := s.resolveProfileConfigurationWithOverrides(ctx, profile, in.ConfigurationOverrides)
	if err != nil {
		return GroupMemberAdmission{}, err
	}
	return s.finishGroupMember(ctx, in, defaults, configuration, current, nil)
}

func (s *Service) finishGroupMember(ctx context.Context, in CreateGroupMemberRequest, defaults model.GroupConfiguration, configuration ResolvedProfileConfiguration, ref *model.ConfigurationProfileRef, sources *model.TeamConfigurationSources) (GroupMemberAdmission, error) {
	desired := configuration.Desired
	var err error
	if err := s.verifyLaunchSandbox(ctx, desired.HostSandbox); err != nil {
		return GroupMemberAdmission{}, err
	}
	desired.Environment, err = model.MergeEnvironment(defaults.Environment, desired.Environment, in.Environment)
	if err != nil {
		return GroupMemberAdmission{}, fail(ErrInvalid, "%v", err)
	}
	if err = validateDesired(desired); err != nil {
		return GroupMemberAdmission{}, err
	}
	desired.HostSandbox = model.SandboxInGroup(desired.HostSandbox, in.GroupID)
	labels, err := s.configurationDisplayLabels(ctx, ref, nil)
	if err != nil {
		return GroupMemberAdmission{}, err
	}
	memberLabels := model.AgentDisplayLabels{Role: labels.Role, Description: labels.Description}
	if in.Labels != nil {
		memberLabels = *in.Labels
	}
	labels = model.AgentLabels{Groups: map[model.GroupID]model.AgentDisplayLabels{in.GroupID: memberLabels}}
	now := s.now().UTC()
	agent := model.Agent{Labels: labels, ID: in.ID, Name: in.Name, Lifecycle: model.AgentActive, Notifications: model.AgentNotificationPreferences{DirectMessage: model.NotificationIfAvailable}, Desired: desired, ConfigurationProfile: ref, Revision: 1, CreatedAt: now, UpdatedAt: now}
	return GroupMemberAdmission{Request: in, Agent: agent, Configuration: configuration, Sources: sources, At: now}, nil
}

// A group without a selected profile uses the same default chain as an inline
// team member. Keep the sources until transactional admission, not just values.
func (s *Service) resolveGroupMemberFallback(ctx context.Context, in CreateGroupMemberRequest, defaults model.GroupConfiguration) (GroupMemberAdmission, error) {
	options := model.ConfigurationOptions{}
	if in.ConfigurationOverrides != nil {
		options = *in.ConfigurationOverrides
	}
	resolved, sources, err := s.resolveInlineConfiguration(ctx, options, in.GroupID)
	if err != nil {
		return GroupMemberAdmission{}, err
	}
	if sources.GroupDefaultsRevision != defaults.Revision || sources.Selected != nil {
		return GroupMemberAdmission{}, ErrConflict
	}
	if err := s.requireProfileCreation(ctx, in.Context.Principal, model.ConfigurationProfile{}); err != nil {
		return GroupMemberAdmission{}, err
	}
	return s.finishGroupMember(ctx, in, defaults, resolved, resolved.GlobalProfile, &sources)
}

func (s *Service) resolveSelectedGroupMember(ctx context.Context, in CreateGroupMemberRequest, defaults model.GroupConfiguration) (GroupMemberAdmission, error) {
	selected, err := s.store.ConfigurationProfile(ctx, in.ProfileID, "")
	if err != nil {
		return GroupMemberAdmission{}, err
	}
	if selected.Profile.Archived {
		return GroupMemberAdmission{}, ErrConflict
	}
	if err := ConfigurationProfileEnabled(selected.Profile); err != nil {
		return GroupMemberAdmission{}, err
	}
	if err := s.requireProfileCreation(ctx, in.Context.Principal, selected.Profile); err != nil {
		return GroupMemberAdmission{}, err
	}
	layers := []model.ConfigurationOptions{}
	sources := model.TeamConfigurationSources{GroupID: in.GroupID, GroupDefaultsRevision: defaults.Revision}
	if defaults.Profile != nil {
		groupProfile, err := s.store.ConfigurationProfile(ctx, defaults.Profile.ProfileID, "")
		if err != nil {
			return GroupMemberAdmission{}, err
		}
		if groupProfile.Profile.Archived {
			return GroupMemberAdmission{}, ErrConflict
		}
		if err := ConfigurationProfileEnabled(groupProfile.Profile); err != nil {
			return GroupMemberAdmission{}, err
		}
		if err := s.requireProfileCreation(ctx, in.Context.Principal, groupProfile.Profile); err != nil {
			return GroupMemberAdmission{}, err
		}
		sources.Selected = &groupProfile.Revision.Ref
		layers = append(layers, profileConfigurationOptions(groupProfile.Revision))
	}
	layers = append(layers, profileConfigurationOptions(selected.Revision))
	resolved, err := s.resolveConfigurationLayers(ctx, selected.Profile.ID, layers, in.ConfigurationOverrides)
	if err != nil {
		return GroupMemberAdmission{}, err
	}
	resolved.Selected = selected.Revision.Ref
	sources.DefaultsRevision, sources.GlobalProfile = resolved.DefaultsRevision, resolved.GlobalProfile
	admission, err := s.finishGroupMember(ctx, in, defaults, resolved, &selected.Revision.Ref, &sources)
	if err != nil || in.Labels != nil {
		return admission, err
	}
	labels, err := s.configurationDisplayLabelTiers(ctx, resolved.GlobalProfile, sources.Selected, &selected.Revision.Ref)
	if err != nil {
		return GroupMemberAdmission{}, err
	}
	admission.Agent.Labels.Groups[in.GroupID] = labels
	return admission, nil
}
