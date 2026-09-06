package model

import "time"

// AccessRequestID identifies one durable, bounded authority request. RequestID
// remains the caller-chosen idempotency key; this ID is the stable resource
// operators inspect and decide.
type AccessRequestID string

func (id AccessRequestID) Validate() error {
	return ValidateStableID("access request id", string(id))
}

type AccessRequestState string

const (
	AccessRequestPending  AccessRequestState = "pending"
	AccessRequestApproved AccessRequestState = "approved"
	AccessRequestDenied   AccessRequestState = "denied"
	AccessRequestExpired  AccessRequestState = "expired"
)

const (
	AccessAnswerApprove = "approve"
	AccessAnswerDeny    = "deny"
)

// AccessRequest is the exact authority proposal authored by an authenticated
// execution. Subject is derived from that execution by the authority owner;
// callers cannot select it. Approval creates this exact grant, expiring at
// ExpiresAt, and cannot edit its action, resource, configuration or bounds.
type AccessRequest struct {
	ID                     AccessRequestID
	DecisionID             DecisionID
	RequestID              RequestID
	Requester              Principal
	Subject                AuthoritySubject
	Action                 Action
	Resource               ResourceSelector
	RequestedConfiguration *DesiredConfiguration
	Bounds                 ConfigurationBounds
	Reason                 string
	RequestedLifetime      time.Duration
	ExpiresAt              time.Time
	State                  AccessRequestState
	GrantID                GrantID
	Revision               Revision
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// AccessDecisionSubmission is an attributable operator answer to the exact
// request revision. RequestID gives the submission its own stable idempotency
// scope, independently of the requester's RequestID.
type AccessDecisionSubmission struct {
	RequestID              RequestID
	DecisionID             DecisionID
	ExpectedWindowRevision Revision
	Answer                 string
	Reason                 string
	Actor                  Principal
	SubmittedAt            time.Time
}

// AccessDecision is the authority-owned form of the shared decision
// projection. It uses the same kind/state/answer vocabulary as work decisions
// while retaining an explicit source request instead of manufacturing a run.
type AccessDecision struct {
	ID               DecisionID
	Kind             DecisionKind
	AccessRequestID  AccessRequestID
	SourceRevision   Revision
	PermittedAnswers []string
	ExpiresAt        time.Time
	State            DecisionState
	Revision         Revision
	Submission       *AccessDecisionSubmission
}
