package transport

import (
	"net/http"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (h *Handler) putDenial(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var body struct {
		Subject          model.AuthoritySubject `json:"subject"`
		Action           model.Action           `json:"action"`
		ExpectedRevision model.Revision         `json:"expected_revision"`
	}
	if !decodeRequest(w, r, &body) {
		return
	}
	result, err := h.authority.PutDenial(r.Context(), app.PutDenialRequest{Principal: p, ExpectedRevision: body.ExpectedRevision, Denial: model.AuthorityDenial{ID: model.DenialID(r.PathValue("id")), Subject: body.Subject, Action: body.Action}})
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result.Denial)
}
func (h *Handler) deleteDenial(w http.ResponseWriter, r *http.Request) {
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
	if err := h.authority.DeleteDenial(r.Context(), app.DeleteDenialRequest{Principal: p, DenialID: model.DenialID(r.PathValue("id")), ExpectedRevision: body.ExpectedRevision}); err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"removed": true})
}
