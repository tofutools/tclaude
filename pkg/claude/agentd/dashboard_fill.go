package agentd

import (
	"encoding/json"
	"net/http"
)

// Form fills (tcl-3ta7bl): a permitted agent proposes values in the operator's
// open form with POST /v1/dashboard/fill. The request travels to the tab like a
// capture and answers through the same route, so the same rules pick the tab and
// a hidden tab is never touched. The page does the work (form-fill.js), through
// each control's own handlers. Nothing here can submit a form: the request only
// carries field values.

const PermDashboardFormsFill = "dashboard.forms.fill"

const (
	fillMaxBody   = 256 << 10
	fillMaxFields = 64
	fillMaxField  = 120
	fillMaxValue  = 16 << 10
)

// fillForms are the forms that opted in. The page holds which of their fields can
// be filled, and refuses the rest.
var fillForms = map[string]bool{"spawn": true, "config": true}

type fillField struct {
	Field string `json:"field"`
	Value string `json:"value"`
}

type fillRequest struct {
	Form   string      `json:"form"`
	Fields []fillField `json:"fields"`
}

// fillOutcome says what became of one field. It carries no value.
type fillOutcome struct {
	Field  string `json:"field"`
	Status string `json:"status"`
	Widens string `json:"widens,omitempty"`
	// Moved names the other fields this change moved, as a form's cascades do.
	Moved []string `json:"moved,omitempty"`
}

func cleanFillOutcomes(outcomes []fillOutcome) []fillOutcome {
	if len(outcomes) > fillMaxFields {
		outcomes = outcomes[:fillMaxFields]
	}
	for i := range outcomes {
		outcomes[i].Field = guideText(outcomes[i].Field, fillMaxField)
		outcomes[i].Status = guideText(outcomes[i].Status, 32)
		outcomes[i].Widens = guideText(outcomes[i].Widens, 240)
		if len(outcomes[i].Moved) > fillMaxFields {
			outcomes[i].Moved = outcomes[i].Moved[:fillMaxFields]
		}
		for j := range outcomes[i].Moved {
			outcomes[i].Moved[j] = guideText(outcomes[i].Moved[j], fillMaxField)
		}
	}
	return outcomes
}

// POST /v1/dashboard/fill — propose values in the operator's open form.
func handleDashboardFill(w http.ResponseWriter, r *http.Request) {
	convID, ok := requirePermission(w, r, PermDashboardFormsFill)
	if !ok {
		return
	}
	var req fillRequest
	r.Body = http.MaxBytesReader(w, r.Body, fillMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid fill request: "+err.Error())
		return
	}
	if !fillForms[req.Form] {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a fill names a form: spawn or config")
		return
	}
	if len(req.Fields) == 0 || len(req.Fields) > fillMaxFields {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a fill needs between 1 and 64 fields")
		return
	}
	for _, field := range req.Fields {
		if field.Field == "" || len(field.Field) > fillMaxField || len(field.Value) > fillMaxValue {
			writeError(w, http.StatusBadRequest, "invalid_arg", "a field needs a name, and a value of at most 16 KiB")
			return
		}
	}
	id, err := newCaptureID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	pending := &pendingCapture{captureRequest: captureRequest{ID: id, Fill: &req}, done: make(chan captureResult, 1)}
	result, ok := awaitTab(w, r, convID, pending)
	if !ok {
		return
	}
	if result.err != "" {
		writeError(w, http.StatusConflict, "fill_failed", result.err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fields": result.fill})
}
