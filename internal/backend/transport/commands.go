package transport

import (
	"context"
	"net/http"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

type commandIdentity struct {
	RequestID model.RequestID `json:"request_id"`
}

func (c commandIdentity) context(p model.Principal) app.RequestContext {
	return app.RequestContext{Principal: p, RequestID: c.RequestID}
}

type agentTarget struct {
	AgentID          model.AgentID  `json:"agent_id"`
	ExpectedRevision model.Revision `json:"expected_revision"`
}

type standaloneTarget struct {
	Desired        model.DesiredConfiguration `json:"desired"`
	ConversationID model.ConversationID       `json:"conversation_id,omitempty"`
}

type launchTarget struct {
	Agent      *agentTarget      `json:"agent,omitempty"`
	Standalone *standaloneTarget `json:"standalone,omitempty"`
}

func (t launchTarget) application() app.LaunchTarget {
	var target app.LaunchTarget
	if t.Agent != nil {
		target.Agent = &app.AgentLaunchTarget{AgentID: t.Agent.AgentID, ExpectedRevision: t.Agent.ExpectedRevision}
	}
	if t.Standalone != nil {
		target.Standalone = &app.StandaloneLaunchTarget{Desired: t.Standalone.Desired, ConversationID: t.Standalone.ConversationID}
	}
	return target
}

type launchBody struct {
	commandIdentity
	InitialMessage string       `json:"initial_message,omitempty"`
	Target         launchTarget `json:"target"`
}

type resumeBody struct {
	commandIdentity
	Target                      launchTarget         `json:"target"`
	ConversationID              model.ConversationID `json:"conversation_id"`
	ExpectedAssociationRevision model.Revision       `json:"expected_association_revision"`
}

type interactBody struct {
	commandIdentity
	ExecutionID model.ExecutionID `json:"execution_id"`
	Text        string            `json:"text"`
}

type stopBody struct {
	commandIdentity
	ExecutionID model.ExecutionID `json:"execution_id"`
	Force       bool              `json:"force"`
}

type contextBody struct {
	commandIdentity
	ExecutionID                 model.ExecutionID         `json:"execution_id"`
	Intent                      ports.ContextChangeIntent `json:"intent"`
	ExpectedConversationID      model.ConversationID      `json:"expected_conversation_id"`
	ExpectedAssociationRevision model.Revision            `json:"expected_association_revision"`
}

// operationHandler translates wire data only. Domain validation, authority,
// durable request replay and effect scheduling belong to the application.
func operationHandler[Body any](h *Handler, call func(context.Context, model.Principal, Body) (app.OperationResult, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body Body
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err := call(r.Context(), p, body)
		if err != nil {
			applicationError(w, err)
			return
		}
		var execution *executionView
		if result.Execution != nil {
			view := projectExecution(*result.Execution)
			execution = &view
		}
		writeJSON(w, http.StatusAccepted, struct {
			Operation operationView  `json:"operation"`
			Execution *executionView `json:"execution,omitempty"`
			Repeated  bool           `json:"repeated"`
		}{projectOperation(result.Operation), execution, result.Repeated})
	}
}

func (h *Handler) registerCommands() {
	h.mux.HandleFunc("POST /v2/launch", operationHandler(h, func(ctx context.Context, p model.Principal, b launchBody) (app.OperationResult, error) {
		return h.application.Launch(ctx, app.LaunchRequest{RequestContext: b.context(p), InitialMessage: b.InitialMessage, Target: b.Target.application()})
	}))
	h.mux.HandleFunc("POST /v2/resume", operationHandler(h, func(ctx context.Context, p model.Principal, b resumeBody) (app.OperationResult, error) {
		return h.application.Resume(ctx, app.ResumeRequest{RequestContext: b.context(p), Target: b.Target.application(),
			ConversationID: b.ConversationID, ExpectedAssociationRevision: b.ExpectedAssociationRevision})
	}))
	h.mux.HandleFunc("POST /v2/interact", operationHandler(h, func(ctx context.Context, p model.Principal, b interactBody) (app.OperationResult, error) {
		return h.application.Interact(ctx, app.InteractRequest{RequestContext: b.context(p), ExecutionID: b.ExecutionID, Text: b.Text})
	}))
	h.mux.HandleFunc("POST /v2/stop", operationHandler(h, func(ctx context.Context, p model.Principal, b stopBody) (app.OperationResult, error) {
		return h.application.Stop(ctx, app.StopRequest{RequestContext: b.context(p), ExecutionID: b.ExecutionID, Force: b.Force})
	}))
	h.mux.HandleFunc("POST /v2/context", operationHandler(h, func(ctx context.Context, p model.Principal, b contextBody) (app.OperationResult, error) {
		return h.application.ChangeContext(ctx, app.ChangeContextRequest{RequestContext: b.context(p), ExecutionID: b.ExecutionID,
			Intent: b.Intent, ExpectedConversationID: b.ExpectedConversationID, ExpectedAssociationRevision: b.ExpectedAssociationRevision})
	}))
	h.mux.HandleFunc("PUT /v2/agents/{id}", h.updateAgent)
	h.mux.HandleFunc("POST /v2/messages", h.sendMessage)
	h.mux.HandleFunc("POST /v2/messages/{id}/read", h.markMessageRead)
	h.mux.HandleFunc("POST /v2/recover", h.recover)
}

func (h *Handler) updateAgent(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body struct {
		Labels        *model.AgentLabels                 `json:"labels"`
		TaskReference string                             `json:"task_reference"`
		Notifications model.AgentNotificationPreferences `json:"notifications"`

		ConfigurationOverrides *model.ConfigurationOptions    `json:"configuration_overrides"`
		ConfigurationProfile   *model.ConfigurationProfileRef `json:"configuration_profile"`
		ConfigurationDefault   string                         `json:"configuration_default"`
		ExpectedRevision       model.Revision                 `json:"expected_revision"`
		Name                   string                         `json:"name"`
		Desired                model.DesiredConfiguration     `json:"desired"`
	}
	if !decodeRequest(w, r, &body) {
		return
	}
	result, err := h.application.UpdateAgent(r.Context(), app.UpdateAgentRequest{Context: p, ID: model.AgentID(r.PathValue("id")),
		ExpectedRevision: body.ExpectedRevision, Name: body.Name, Desired: body.Desired, ConfigurationProfile: body.ConfigurationProfile, ConfigurationDefault: body.ConfigurationDefault, ConfigurationOverrides: body.ConfigurationOverrides, TaskReference: body.TaskReference, Labels: body.Labels, Notifications: body.Notifications})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectAgent(result.Agent))
}

func (h *Handler) sendMessage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body struct {
		commandIdentity
		Subject         string                `json:"subject"`
		ParentMessageID model.MessageID       `json:"parent_message_id"`
		To              model.MessageAudience `json:"to"`
		CC              model.MessageAudience `json:"cc"`
		Attachments     []app.AttachmentInput `json:"attachments"`
		Recipients      []model.AgentID       `json:"recipients"`
		Body            string                `json:"body"`
	}
	if !decodeBoundedRequest(w, r, &body, 16<<20) {
		return
	}
	result, err := h.application.SendMessage(r.Context(), app.SendMessageRequest{RequestContext: body.context(p), RecipientAgentIDs: body.Recipients, Body: body.Body, Subject: body.Subject, ParentMessageID: body.ParentMessageID, To: body.To, CC: body.CC, Attachments: body.Attachments})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, projectMessage(result.Message))
}

func (h *Handler) markMessageRead(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body struct {
		Operator bool `json:"operator"`
		commandIdentity
		AgentID model.AgentID `json:"agent_id"`
	}
	if !decodeRequest(w, r, &body) {
		return
	}
	result, err := h.application.MarkMessageRead(r.Context(), app.MarkMessageReadRequest{RequestContext: body.context(p),
		MessageID: model.MessageID(r.PathValue("id")), AgentID: body.AgentID, Operator: body.Operator})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectMessage(result.Message))
}

func (h *Handler) recover(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body struct{}
	if !decodeRequest(w, r, &body) {
		return
	}
	result, err := h.application.Recover(r.Context(), app.RecoverRequest{Principal: p})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
