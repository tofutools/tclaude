package transport

import (
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"net/http"
)

func (h *Handler) presentation(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	var result app.PresentationResult
	var err error
	if r.Method == http.MethodGet {
		result, err = h.application.ReadPresentation(r.Context(), principal)
	} else {
		var body struct {
			Preferences      model.PresentationPreferences `json:"preferences"`
			ExpectedRevision model.Revision                `json:"expected_revision"`
		}
		if !decodeRequest(w, r, &body) {
			return
		}
		result, err = h.application.PutPresentation(r.Context(), app.PutPresentationRequest{Principal: principal, Preferences: body.Preferences, ExpectedRevision: body.ExpectedRevision})
	}
	if err != nil {
		applicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
