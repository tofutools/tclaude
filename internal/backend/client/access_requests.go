package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

type Actor struct {
	Kind          model.PrincipalKind `json:"kind"`
	AgentID       model.AgentID       `json:"agent_id,omitempty"`
	ExecutionID   model.ExecutionID   `json:"execution_id,omitempty"`
	AutomationRun string              `json:"automation_run,omitempty"`
}

type AccessRequestView struct {
	ID                     model.AccessRequestID       `json:"id"`
	RequestID              model.RequestID             `json:"request_id"`
	Requester              Actor                       `json:"requester"`
	Subject                model.AuthoritySubject      `json:"subject"`
	Action                 model.Action                `json:"action"`
	Resource               model.ResourceSelector      `json:"resource"`
	RequestedConfiguration *model.DesiredConfiguration `json:"requested_configuration,omitempty"`
	Bounds                 model.ConfigurationBounds   `json:"bounds"`
	Reason                 string                      `json:"reason"`
	ExpiresAt              time.Time                   `json:"expires_at"`
	State                  model.AccessRequestState    `json:"state"`
	GrantID                model.GrantID               `json:"grant_id,omitempty"`
	Revision               model.Revision              `json:"revision"`
	CreatedAt              time.Time                   `json:"created_at"`
	UpdatedAt              time.Time                   `json:"updated_at"`
}

type AccessDecisionSubmissionView struct {
	RequestID              model.RequestID `json:"request_id"`
	ExpectedWindowRevision model.Revision  `json:"expected_window_revision"`
	Answer                 string          `json:"answer"`
	Reason                 string          `json:"reason"`
	Actor                  Actor           `json:"actor"`
	SubmittedAt            time.Time       `json:"submitted_at"`
}

type AccessDecisionView struct {
	DecisionID       model.DecisionID              `json:"decision_id"`
	Kind             model.DecisionKind            `json:"kind"`
	SourceType       string                        `json:"source_type"`
	AccessRequestID  model.AccessRequestID         `json:"access_request_id"`
	SourceRevision   model.Revision                `json:"source_revision"`
	PermittedAnswers []string                      `json:"permitted_answers"`
	ExpiresAt        time.Time                     `json:"expires_at"`
	State            model.DecisionState           `json:"state"`
	Revision         model.Revision                `json:"revision"`
	Submission       *AccessDecisionSubmissionView `json:"submission,omitempty"`
}

type AccessRequestResult struct {
	Request  AccessRequestView  `json:"request"`
	Decision AccessDecisionView `json:"decision"`
	Repeated bool               `json:"repeated"`
}

type RequestAccessInput struct {
	RequestID              model.RequestID             `json:"request_id"`
	Action                 model.Action                `json:"action"`
	Resource               model.ResourceSelector      `json:"resource"`
	RequestedConfiguration *model.DesiredConfiguration `json:"requested_configuration,omitempty"`
	Bounds                 model.ConfigurationBounds   `json:"bounds"`
	Reason                 string                      `json:"reason"`
	LifetimeSeconds        int64                       `json:"lifetime_seconds"`
}

type DecideAccessInput struct {
	RequestID              model.RequestID `json:"request_id"`
	ExpectedWindowRevision model.Revision  `json:"expected_window_revision"`
	Answer                 string          `json:"answer"`
	Reason                 string          `json:"reason"`
}

func (c *Client) RequestAccess(ctx context.Context, input RequestAccessInput) (AccessRequestResult, error) {
	var result AccessRequestResult
	err := c.Call(ctx, http.MethodPost, "/v2/access-requests", input, &result)
	return result, err
}

func (c *Client) ListAccessRequests(ctx context.Context, pendingOnly bool) ([]AccessRequestResult, error) {
	path := "/v2/access-requests"
	if pendingOnly {
		path += "?pending_only=" + strconv.FormatBool(true)
	}
	var result struct {
		Requests []AccessRequestResult `json:"requests"`
	}
	err := c.Call(ctx, http.MethodGet, path, nil, &result)
	return result.Requests, err
}

func (c *Client) GetAccessRequest(ctx context.Context, id model.AccessRequestID) (AccessRequestResult, error) {
	var result AccessRequestResult
	err := c.Call(ctx, http.MethodGet, "/v2/access-requests/"+url.PathEscape(string(id)), nil, &result)
	return result, err
}

func (c *Client) DecideAccessRequest(ctx context.Context, id model.AccessRequestID, input DecideAccessInput) (AccessRequestResult, error) {
	var result AccessRequestResult
	err := c.Call(ctx, http.MethodPost, "/v2/access-requests/"+url.PathEscape(string(id))+"/decision", input, &result)
	return result, err
}
