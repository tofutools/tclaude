package transport

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// Actor attribution is public; authentication generations and delegated
// authority are not part of correspondence or lifecycle projections.
type collaborationActor struct {
	Kind          model.PrincipalKind
	AgentID       model.AgentID
	ExecutionID   model.ExecutionID
	AutomationRun string
}
type messageView struct {
	model.Message
	Sender collaborationActor
}
type agentView struct {
	model.Agent
	RetiredBy collaborationActor
}

func projectCollaborationActor(p model.Principal) collaborationActor {
	return collaborationActor{p.Kind, p.AgentID, p.ExecutionID, p.AutomationRun}
}
func projectMessage(m model.Message) messageView {
	return messageView{m, projectCollaborationActor(m.Sender)}
}
func projectAgent(a model.Agent) agentView {
	return agentView{a, projectCollaborationActor(a.RetiredBy)}
}
func projectMessages(messages []model.Message) []messageView {
	out := make([]messageView, 0, len(messages))
	for _, m := range messages {
		out = append(out, projectMessage(m))
	}
	return out
}
func projectAgents(agents []model.Agent) []agentView {
	out := make([]agentView, 0, len(agents))
	for _, a := range agents {
		out = append(out, projectAgent(a))
	}
	return out
}

func (h *Handler) registerCollaboration() {
	h.mux.HandleFunc("POST /v2/agents/{id}/retire", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			ExpectedRevision model.Revision `json:"expected_revision"`
			Reason           string         `json:"reason"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err := h.application.RetireAgent(r.Context(), app.RetireAgentRequest{Context: p, ID: model.AgentID(r.PathValue("id")), ExpectedRevision: body.ExpectedRevision, Reason: body.Reason})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, projectAgent(result.Agent))
	})
	h.mux.HandleFunc("POST /v2/agents/{id}/reactivate", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			ExpectedRevision model.Revision `json:"expected_revision"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err := h.application.ReactivateAgent(r.Context(), app.ReactivateAgentRequest{Context: p, ID: model.AgentID(r.PathValue("id")), ExpectedRevision: body.ExpectedRevision})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, projectAgent(result.Agent))
	})
	h.mux.HandleFunc("POST /v2/attachment-claims", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		var body struct {
			Filename  string `json:"filename"`
			MediaType string `json:"media_type"`
			Content   []byte `json:"content"`
		}
		// The application bounds decoded bytes at 5 MiB. Permit its base64 JSON
		// envelope here without raising the bound for ordinary mutations.
		r.Body = http.MaxBytesReader(w, r.Body, 7<<20)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		result, err := h.application.CreateAttachmentClaim(r.Context(), app.CreateAttachmentClaimRequest{Principal: p, Filename: body.Filename, MediaType: body.MediaType, Content: body.Content})
		if err != nil {
			applicationError(w, err)
			return
		}
		// Claim owner is resolved from the authenticated caller and is not echoed.
		writeJSON(w, http.StatusCreated, struct {
			ID         model.AttachmentClaimID
			Attachment model.Attachment
			ExpiresAt  time.Time
		}{result.Claim.ID, result.Claim.Attachment, result.Claim.ExpiresAt})
	})
	h.mux.HandleFunc("GET /v2/attachments/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.caller(w, r)
		if !ok {
			return
		}
		result, err := h.application.ReadAttachment(r.Context(), app.ReadAttachmentRequest{Principal: p, AttachmentID: model.AttachmentID(r.PathValue("id"))})
		if err != nil {
			applicationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
