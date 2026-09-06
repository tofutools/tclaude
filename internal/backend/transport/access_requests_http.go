package transport

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

type accessRequestView struct {
	ID                     model.AccessRequestID       `json:"id"`
	RequestID              model.RequestID             `json:"request_id"`
	Requester              workActor                   `json:"requester"`
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

type accessDecisionSubmissionView struct {
	RequestID              model.RequestID `json:"request_id"`
	ExpectedWindowRevision model.Revision  `json:"expected_window_revision"`
	Answer                 string          `json:"answer"`
	Reason                 string          `json:"reason"`
	Actor                  workActor       `json:"actor"`
	SubmittedAt            time.Time       `json:"submitted_at"`
}

type accessDecisionView struct {
	DecisionID       model.DecisionID              `json:"decision_id"`
	Kind             model.DecisionKind            `json:"kind"`
	SourceType       string                        `json:"source_type"`
	AccessRequestID  model.AccessRequestID         `json:"access_request_id"`
	SourceRevision   model.Revision                `json:"source_revision"`
	PermittedAnswers []string                      `json:"permitted_answers"`
	ExpiresAt        time.Time                     `json:"expires_at"`
	State            model.DecisionState           `json:"state"`
	Revision         model.Revision                `json:"revision"`
	Submission       *accessDecisionSubmissionView `json:"submission,omitempty"`
}

type accessRequestResultView struct {
	Request  accessRequestView  `json:"request"`
	Decision accessDecisionView `json:"decision"`
	Repeated bool               `json:"repeated"`
}

func projectAccessRequest(result app.AccessRequestResult) accessRequestResultView {
	request := result.Request
	requestView := accessRequestView{
		ID: request.ID, RequestID: request.RequestID, Requester: projectWorkActor(request.Requester),
		Subject: request.Subject, Action: request.Action, Resource: request.Resource,
		RequestedConfiguration: request.RequestedConfiguration, Bounds: request.Bounds,
		Reason: request.Reason, ExpiresAt: request.ExpiresAt, State: request.State,
		Revision: request.Revision, CreatedAt: request.CreatedAt, UpdatedAt: request.UpdatedAt,
	}
	if request.State == model.AccessRequestApproved {
		requestView.GrantID = request.GrantID
	}
	decision := result.Decision
	decisionView := accessDecisionView{
		DecisionID: decision.ID, Kind: decision.Kind, SourceType: "access_request",
		AccessRequestID: decision.AccessRequestID, SourceRevision: decision.SourceRevision,
		PermittedAnswers: decision.PermittedAnswers, ExpiresAt: decision.ExpiresAt,
		State: decision.State, Revision: decision.Revision,
	}
	if decision.Submission != nil {
		submission := decision.Submission
		decisionView.Submission = &accessDecisionSubmissionView{
			RequestID: submission.RequestID, ExpectedWindowRevision: submission.ExpectedWindowRevision,
			Answer: submission.Answer, Reason: submission.Reason, Actor: projectWorkActor(submission.Actor),
			SubmittedAt: submission.SubmittedAt,
		}
	}
	return accessRequestResultView{Request: requestView, Decision: decisionView, Repeated: result.Repeated}
}

func (h *Handler) RegisterAccessRequestAPI(api app.AccessRequestAPI) error {
	if api == nil {
		return errors.New("access request application interface is required")
	}
	if h.accessRequests != nil {
		return errors.New("access request API already registered")
	}
	h.accessRequests = api
	h.mux.HandleFunc("POST /v2/access-requests", h.requestAccess)
	h.mux.HandleFunc("GET /v2/access-requests", h.listAccessRequests)
	h.mux.HandleFunc("GET /v2/access-requests/{id}", h.getAccessRequest)
	h.mux.HandleFunc("POST /v2/access-requests/{id}/decision", h.decideAccessRequest)
	return nil
}

func (h *Handler) requestAccess(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body struct {
		commandIdentity
		Action                 model.Action                `json:"action"`
		Resource               model.ResourceSelector      `json:"resource"`
		RequestedConfiguration *model.DesiredConfiguration `json:"requested_configuration,omitempty"`
		Bounds                 model.ConfigurationBounds   `json:"bounds"`
		Reason                 string                      `json:"reason"`
		LifetimeSeconds        int64                       `json:"lifetime_seconds"`
	}
	if !decodeRequest(w, r, &body) {
		return
	}
	if body.LifetimeSeconds < 1 || body.LifetimeSeconds > int64(app.MaxAccessRequestLifetime/time.Second) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	result, err := h.accessRequests.RequestAccess(r.Context(), app.RequestAccessRequest{
		Context: body.context(principal), Action: body.Action, Resource: body.Resource,
		RequestedConfiguration: body.RequestedConfiguration, Bounds: body.Bounds,
		Reason: body.Reason, Lifetime: time.Duration(body.LifetimeSeconds) * time.Second,
	})
	if err != nil {
		applicationError(w, err)
		return
	}
	status := http.StatusCreated
	if result.Repeated {
		status = http.StatusOK
	}
	writeJSON(w, status, projectAccessRequest(result))
}

func (h *Handler) listAccessRequests(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	pending := false
	query := r.URL.Query()
	if len(query) > 0 {
		if len(query) != 1 || len(query["pending_only"]) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		var err error
		pending, err = strconv.ParseBool(query.Get("pending_only"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	result, err := h.accessRequests.ListAccessRequests(r.Context(), app.ListAccessRequestsRequest{Principal: principal, PendingOnly: pending})
	if err != nil {
		applicationError(w, err)
		return
	}
	views := make([]accessRequestResultView, 0, len(result.Requests))
	for _, request := range result.Requests {
		views = append(views, projectAccessRequest(request))
	}
	writeJSON(w, http.StatusOK, struct {
		Requests []accessRequestResultView `json:"requests"`
	}{views})
}

func (h *Handler) getAccessRequest(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	result, err := h.accessRequests.GetAccessRequest(r.Context(), app.GetAccessRequestRequest{Principal: principal, ID: model.AccessRequestID(r.PathValue("id"))})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectAccessRequest(result))
}

func (h *Handler) decideAccessRequest(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body struct {
		commandIdentity
		ExpectedWindowRevision model.Revision `json:"expected_window_revision"`
		Answer                 string         `json:"answer"`
		Reason                 string         `json:"reason"`
	}
	if !decodeRequest(w, r, &body) {
		return
	}
	record, err := h.accessRequests.GetAccessRequest(r.Context(), app.GetAccessRequestRequest{Principal: principal, ID: model.AccessRequestID(r.PathValue("id"))})
	if err != nil {
		applicationError(w, err)
		return
	}
	result, err := h.accessRequests.DecideAccessRequest(r.Context(), app.DecideAccessRequestRequest{
		Context: body.context(principal), DecisionID: record.Decision.ID,
		ExpectedWindowRevision: body.ExpectedWindowRevision, Answer: body.Answer, Reason: body.Reason,
	})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectAccessRequest(result))
}
