package app

import (
	"context"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

const MaxAccessRequestLifetime = 5 * time.Minute

type RequestAccessRequest struct {
	Context                RequestContext
	Action                 model.Action
	Resource               model.ResourceSelector
	RequestedConfiguration *model.DesiredConfiguration
	Bounds                 model.ConfigurationBounds
	Reason                 string
	Lifetime               time.Duration
}

type ListAccessRequestsRequest struct {
	Principal   model.Principal
	PendingOnly bool
}

type GetAccessRequestRequest struct {
	Principal model.Principal
	ID        model.AccessRequestID
}

type DecideAccessRequestRequest struct {
	Context                RequestContext
	DecisionID             model.DecisionID
	ExpectedWindowRevision model.Revision
	Answer                 string
	Reason                 string
}

type AccessRequestResult struct {
	Request  model.AccessRequest
	Decision model.AccessDecision
	Repeated bool
}

type AccessRequestListResult struct {
	Requests []AccessRequestResult
}

// AccessRequestAPI is deliberately separate from broad agent and authority
// administration interfaces. The same implementation serves execution-owned
// request/query and operator-owned all-query/decision paths.
type AccessRequestAPI interface {
	RequestAccess(context.Context, RequestAccessRequest) (AccessRequestResult, error)
	ListAccessRequests(context.Context, ListAccessRequestsRequest) (AccessRequestListResult, error)
	GetAccessRequest(context.Context, GetAccessRequestRequest) (AccessRequestResult, error)
	DecideAccessRequest(context.Context, DecideAccessRequestRequest) (AccessRequestResult, error)
}

// AccessRequestStore is the authority owner's narrow transactional port.
// SQLite implements request admission and approval+grant as single commits.
type AccessRequestStore interface {
	CreateAccessRequest(context.Context, model.AccessRequest, time.Time) (AccessRequestResult, error)
	ListAccessRequests(context.Context, model.Principal, bool, time.Time) ([]AccessRequestResult, error)
	AccessRequest(context.Context, model.AccessRequestID, model.Principal, time.Time) (AccessRequestResult, error)
	DecideAccessRequest(context.Context, model.AccessDecisionSubmission, time.Time) (AccessRequestResult, error)
}

func (s *Service) accessRequestStore() (AccessRequestStore, error) {
	store, ok := any(s.store).(AccessRequestStore)
	if !ok {
		return nil, fail(ErrUnsupported, "access requests are not supported by this store")
	}
	return store, nil
}

func (s *Service) RequestAccess(ctx context.Context, req RequestAccessRequest) (AccessRequestResult, error) {
	if err := req.Bounds.ValidateEnvironments(); err != nil {
		return AccessRequestResult{}, fail(ErrInvalid, "%v", err)
	}
	if req.Context.Principal.Kind != model.PrincipalExecution {
		return AccessRequestResult{}, fail(ErrUnauthorized, "execution identity required")
	}
	if err := req.Context.RequestID.Validate(); err != nil {
		return AccessRequestResult{}, fail(ErrInvalid, "%v", err)
	}
	if req.Action == "" || strings.TrimSpace(req.Reason) == "" || len(req.Reason) > 1024 {
		return AccessRequestResult{}, fail(ErrInvalid, "action and a reason of at most 1024 bytes are required")
	}
	if req.Lifetime < time.Second || req.Lifetime > MaxAccessRequestLifetime {
		return AccessRequestResult{}, fail(ErrInvalid, "access request lifetime must be between 1 second and %s", MaxAccessRequestLifetime)
	}
	if req.RequestedConfiguration != nil {
		if err := validateDesired(*req.RequestedConfiguration); err != nil {
			return AccessRequestResult{}, err
		}
	} else if AccessRequestConfigurationRequired(req.Action) {
		return AccessRequestResult{}, fail(ErrInvalid, "%s requires an exact requested configuration and complete bounds", req.Action)
	}
	store, err := s.accessRequestStore()
	if err != nil {
		return AccessRequestResult{}, err
	}
	now := s.now().UTC()
	request := model.AccessRequest{
		ID: model.AccessRequestID(s.newID("access_")), DecisionID: model.DecisionID(s.newID("decision_")),
		GrantID: model.GrantID(s.newID("grant_")), RequestID: req.Context.RequestID,
		Requester: req.Context.Principal, Action: req.Action, Resource: req.Resource,
		RequestedConfiguration: req.RequestedConfiguration, Bounds: req.Bounds,
		Reason: req.Reason, RequestedLifetime: req.Lifetime, ExpiresAt: now.Add(req.Lifetime),
		State: model.AccessRequestPending, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	return store.CreateAccessRequest(ctx, request, now)
}

func (s *Service) ListAccessRequests(ctx context.Context, req ListAccessRequestsRequest) (AccessRequestListResult, error) {
	if req.Principal.Kind != model.PrincipalOperator && req.Principal.Kind != model.PrincipalExecution {
		return AccessRequestListResult{}, fail(ErrUnauthorized, "operator or execution identity required")
	}
	store, err := s.accessRequestStore()
	if err != nil {
		return AccessRequestListResult{}, err
	}
	requests, err := store.ListAccessRequests(ctx, req.Principal, req.PendingOnly, s.now().UTC())
	return AccessRequestListResult{Requests: requests}, err
}

func (s *Service) GetAccessRequest(ctx context.Context, req GetAccessRequestRequest) (AccessRequestResult, error) {
	if req.Principal.Kind != model.PrincipalOperator && req.Principal.Kind != model.PrincipalExecution {
		return AccessRequestResult{}, fail(ErrUnauthorized, "operator or execution identity required")
	}
	if err := req.ID.Validate(); err != nil {
		return AccessRequestResult{}, fail(ErrInvalid, "%v", err)
	}
	store, err := s.accessRequestStore()
	if err != nil {
		return AccessRequestResult{}, err
	}
	return store.AccessRequest(ctx, req.ID, req.Principal, s.now().UTC())
}

func (s *Service) DecideAccessRequest(ctx context.Context, req DecideAccessRequestRequest) (AccessRequestResult, error) {
	if err := requireOperator(req.Context.Principal); err != nil {
		return AccessRequestResult{}, err
	}
	if err := req.Context.RequestID.Validate(); err != nil {
		return AccessRequestResult{}, fail(ErrInvalid, "%v", err)
	}
	if err := req.DecisionID.Validate(); err != nil {
		return AccessRequestResult{}, fail(ErrInvalid, "%v", err)
	}
	if req.ExpectedWindowRevision == 0 || (req.Answer != model.AccessAnswerApprove && req.Answer != model.AccessAnswerDeny) || strings.TrimSpace(req.Reason) == "" || len(req.Reason) > 1024 {
		return AccessRequestResult{}, fail(ErrInvalid, "expected revision, approve/deny answer and a reason of at most 1024 bytes are required")
	}
	store, err := s.accessRequestStore()
	if err != nil {
		return AccessRequestResult{}, err
	}
	now := s.now().UTC()
	return store.DecideAccessRequest(ctx, model.AccessDecisionSubmission{
		RequestID: req.Context.RequestID, DecisionID: req.DecisionID,
		ExpectedWindowRevision: req.ExpectedWindowRevision, Answer: req.Answer,
		Reason: req.Reason, Actor: req.Context.Principal, SubmittedAt: now,
	}, now)
}

// AccessRequestConfigurationRequired identifies application actions whose
// ordinary effects always carry DesiredConfiguration to the evaluator.
func AccessRequestConfigurationRequired(action model.Action) bool {
	return action == model.ActionLaunch || action == model.ActionUpdateConfiguration || action == model.ActionStartWork
}
