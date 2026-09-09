package app

import (
	"context"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// ActionAuthenticator resolves bearer material into exact execution identity.
// It never caches or returns authorization grants; every application operation
// evaluates current authority independently after authentication.
type ActionAuthenticator interface {
	AuthenticateAction(context.Context, []byte) (model.Principal, error)
}

type WhoAmIRequest struct {
	Principal model.Principal
}

type WhoAmIResult struct {
	Principal           model.Principal
	Agent               *model.Agent
	Execution           model.Execution
	CurrentConversation *model.ConversationAssociation
	ContextReadiness    model.ContextReadiness
	EffectiveActions    []model.Action
	EvaluatedAt         time.Time
}

type ReadInboxRequest struct {
	Principal  model.Principal
	UnreadOnly bool
}

type InboxResult struct {
	Messages []model.Message
}

type ReadStatusRequest struct {
	Principal model.Principal
	Target    model.ResourceSelector
}

type StatusResult struct {
	Agents       []model.Agent
	Executions   []model.Execution
	Associations []model.ConversationAssociation
}

type PutGrantRequest struct {
	Principal        model.Principal
	Grant            model.AuthorityGrant
	ExpectedRevision model.Revision
}

type GrantResult struct {
	Grant model.AuthorityGrant
}

type ListAuthorityRequest struct {
	Principal model.Principal
}

type AuthorityStateResult struct {
	Denials     []model.AuthorityDenial
	Grants      []model.AuthorityGrant
	Roles       []model.Role
	Assignments []model.RoleAssignment
}

type DeleteGrantRequest struct {
	Principal        model.Principal
	GrantID          model.GrantID
	ExpectedRevision model.Revision
}

type PutRoleRequest struct {
	Principal        model.Principal
	Role             model.Role
	ExpectedRevision model.Revision
}

type RoleResult struct {
	Role model.Role
}

type PutRoleAssignmentRequest struct {
	Principal        model.Principal
	Assignment       model.RoleAssignment
	ExpectedRevision model.Revision
}

type RoleAssignmentResult struct {
	Assignment model.RoleAssignment
}

type DeleteRoleAssignmentRequest struct {
	Principal        model.Principal
	Assignment       model.RoleAssignment
	ExpectedRevision model.Revision
}

type SetGroupOwnerRequest struct {
	Principal             model.Principal
	GroupID               model.GroupID
	OwnerAgentID          model.AgentID
	OwnerAgentIDs         []model.AgentID
	Bounds                model.ConfigurationBounds
	ExpectedGroupRevision model.Revision
}

type ExecutionAccessStatusRequest struct {
	Principal   model.Principal
	ExecutionID model.ExecutionID
}

type ExecutionAccessStatusResult struct {
	Access model.ExecutionAccessBinding
}

type RevokeExecutionAccessRequest struct {
	Principal        model.Principal
	ExecutionID      model.ExecutionID
	ExpectedRevision model.Revision
}

type RenewExecutionAccessRequest struct {
	ExecutionID      model.ExecutionID
	ExpectedRevision model.Revision
}

type ExecutionAccessRenewalFailure struct {
	ExecutionID model.ExecutionID
	Code        string
	Detail      string
}

type ExecutionAccessRenewalReport struct {
	Renewed []model.ExecutionAccessBinding
	Failed  []ExecutionAccessRenewalFailure
}

// ExecutionAccessLifecycle is a trusted composition port for the host renewal
// worker. It is not registered on the public agent API.
type ExecutionAccessLifecycle interface {
	RenewExecutionAccess(context.Context, RenewExecutionAccessRequest) (ExecutionAccessStatusResult, error)
	SweepExecutionAccess(context.Context) ExecutionAccessRenewalReport
}

type AuthorityExplanationRequest struct {
	Principal              model.Principal
	Action                 model.Action
	Resource               model.ResourceSelector
	RequestedConfiguration *model.DesiredConfiguration
}

type AuthorityExplanationResult struct {
	Decision model.AuthorityDecision
}

// AgentAPI is the execution-authenticated application surface consumed by the
// transport owner. Implementations derive self/inbox identity from Principal;
// request bodies never supply an acting AgentID.
type AgentAPI interface {
	ActionAuthenticator
	WhoAmI(context.Context, WhoAmIRequest) (WhoAmIResult, error)
	ReadInbox(context.Context, ReadInboxRequest) (InboxResult, error)
	ReadStatus(context.Context, ReadStatusRequest) (StatusResult, error)
	ExplainAuthority(context.Context, AuthorityExplanationRequest) (AuthorityExplanationResult, error)
}

// AuthorityAdminAPI is the typed operator-only configuration surface. Owner
// role assignments are visible records evaluated by the same path as direct
// grants; SetGroupOwner does not create a hidden owner bypass.
type AuthorityAdminAPI interface {
	PutDenial(context.Context, PutDenialRequest) (DenialResult, error)
	DeleteDenial(context.Context, DeleteDenialRequest) error
	ListAuthority(context.Context, ListAuthorityRequest) (AuthorityStateResult, error)
	PutGrant(context.Context, PutGrantRequest) (GrantResult, error)
	DeleteGrant(context.Context, DeleteGrantRequest) error
	PutRole(context.Context, PutRoleRequest) (RoleResult, error)
	PutRoleAssignment(context.Context, PutRoleAssignmentRequest) (RoleAssignmentResult, error)
	DeleteRoleAssignment(context.Context, DeleteRoleAssignmentRequest) error
	SetGroupOwner(context.Context, SetGroupOwnerRequest) (GroupResult, error)
	ExecutionAccessStatus(context.Context, ExecutionAccessStatusRequest) (ExecutionAccessStatusResult, error)
	RevokeExecutionAccess(context.Context, RevokeExecutionAccessRequest) (ExecutionAccessStatusResult, error)
}
