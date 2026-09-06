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
	Agents     []model.Agent
	Executions []model.Execution
}

type PutGrantRequest struct {
	Principal        model.Principal
	Grant            model.AuthorityGrant
	ExpectedRevision model.Revision
}

type GrantResult struct {
	Grant model.AuthorityGrant
}

type DeleteGrantRequest struct {
	Principal        model.Principal
	GrantID          model.GrantID
	ExpectedRevision model.Revision
}

type AuthorityExplanationRequest struct {
	Principal model.Principal
	Action    model.Action
	Resource  model.ResourceSelector
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
