package agentd

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Dashboard standing-order mutation routes. This is intentionally a
// dashboard-only, human-authorized surface: agent-side proposals and
// permission-gated activation remain a separate workflow.
func registerDashboardStandingOrderRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/standing-orders", handleDashboardStandingOrderCreate)
	mux.HandleFunc("/api/standing-orders/", handleDashboardStandingOrderAPI)
}

type dashboardStandingOrderMutation struct {
	Name            string   `json:"name"`
	Revision        int64    `json:"revision,omitempty"`
	UpdatedAt       string   `json:"updated_at,omitempty"`
	Target          string   `json:"target"`
	Role            string   `json:"role,omitempty"`
	Summary         string   `json:"summary"`
	Sources         []string `json:"sources,omitempty"`
	Timing          string   `json:"timing"`
	Cadence         string   `json:"cadence"`
	CooldownSeconds int64    `json:"cooldown_seconds,omitempty"`
	Enabled         *bool    `json:"enabled,omitempty"`
	TriggerEvent    string   `json:"trigger_event,omitempty"`
	MatchField      string   `json:"match_field,omitempty"`
	MatchRegex      string   `json:"match_regex,omitempty"`
}

func handleDashboardStandingOrderCreate(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST")
		return
	}
	order, _, _, ok := decodeDashboardStandingOrder(w, r, nil)
	if !ok {
		return
	}
	id, err := db.InsertStandingOrder(order)
	if err != nil {
		writeDashboardStandingOrderError(w, "create", err)
		return
	}
	writeDashboardStandingOrder(w, id)
}

func handleDashboardStandingOrderAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/standing-orders/"), "/")
	parts := strings.SplitN(rest, "/", 2)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "id must be a positive integer")
		return
	}
	order, err := db.GetStandingOrder(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "lookup: "+err.Error())
		return
	}
	if order == nil {
		writeError(w, http.StatusNotFound, "not_found", "standing order not found")
		return
	}
	if len(parts) == 2 {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method", "POST")
			return
		}
		var enabled bool
		switch parts[1] {
		case "enable":
			enabled = true
		case "disable":
			enabled = false
		default:
			writeError(w, http.StatusNotFound, "not_found", "unknown standing-order action")
			return
		}
		revision, updatedAt, ok := dashboardStandingOrderCASFromQuery(w, r)
		if !ok {
			return
		}
		if err := db.SetStandingOrderEnabled(id, enabled, revision, updatedAt); err != nil {
			writeDashboardStandingOrderError(w, "toggle", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		replacement, revision, updatedAt, ok := decodeDashboardStandingOrder(w, r, order)
		if !ok {
			return
		}
		if err := db.UpdateStandingOrder(id, revision, updatedAt, replacement); err != nil {
			writeDashboardStandingOrderError(w, "update", err)
			return
		}
		writeDashboardStandingOrder(w, id)
	case http.MethodDelete:
		revision, updatedAt, ok := dashboardStandingOrderCASFromQuery(w, r)
		if !ok {
			return
		}
		if err := db.DeleteStandingOrder(id, revision, updatedAt); err != nil {
			writeDashboardStandingOrderError(w, "delete", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "PATCH or DELETE")
	}
}

func decodeDashboardStandingOrder(
	w http.ResponseWriter, r *http.Request, existing *db.StandingOrder,
) (*db.StandingOrder, int64, time.Time, bool) {
	creating := existing == nil
	var body dashboardStandingOrderMutation
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "bad body: "+err.Error())
		return nil, 0, time.Time{}, false
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "name is required")
		return nil, 0, time.Time{}, false
	}
	if err := validateCronName(body.Name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return nil, 0, time.Time{}, false
	}
	var expectedUpdatedAt time.Time
	if !creating && body.Revision <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "revision is required for edit")
		return nil, 0, time.Time{}, false
	}
	if !creating {
		var err error
		expectedUpdatedAt, err = time.Parse(time.RFC3339Nano, strings.TrimSpace(body.UpdatedAt))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "updated_at is required for edit")
			return nil, 0, time.Time{}, false
		}
	}
	body.Target = strings.TrimSpace(body.Target)
	if body.Target == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "target is required")
		return nil, 0, time.Time{}, false
	}
	target, err := resolveCronTarget(body.Target)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "resolve target: "+err.Error())
		return nil, 0, time.Time{}, false
	}
	if target.Kind != db.StandingTargetGroup && target.Agent == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"target must resolve to an enrolled agent with a stable agt_ id")
		return nil, 0, time.Time{}, false
	}
	role := strings.TrimSpace(body.Role)
	if strings.EqualFold(role, "all") {
		role = ""
	}
	if role != "" && target.Kind != db.StandingTargetGroup {
		writeError(w, http.StatusBadRequest, "invalid_arg", "role is only valid for a group target")
		return nil, 0, time.Time{}, false
	}
	trigger := strings.TrimSpace(body.TriggerEvent)
	if trigger == "" {
		trigger = db.StandingTriggerSessionStart
	}
	enabled := true
	if existing != nil {
		enabled = existing.Enabled
	}
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	order := &db.StandingOrder{
		Name:             body.Name,
		Summary:          body.Summary,
		TriggerEvent:     trigger,
		TriggerSources:   body.Sources,
		MatchField:       body.MatchField,
		MatchRegex:       body.MatchRegex,
		Timing:           body.Timing,
		Cadence:          body.Cadence,
		CooldownSeconds:  body.CooldownSeconds,
		Enabled:          enabled,
		OperatorAuthored: true,
	}
	if existing != nil {
		// Editing content is not an authorship transfer. In particular, retain
		// agent ownership so retiring that author still disables the order, and
		// never relabel agent-authored guidance as the human's instruction.
		order.OwnerAgent = existing.OwnerAgent
		order.OperatorAuthored = existing.OperatorAuthored
		order.DisabledReason = existing.DisabledReason
		// Explicitly re-enabling is the acknowledgement that clears an
		// automatic retirement marker. An unrelated edit must preserve it.
		if body.Enabled != nil && *body.Enabled {
			order.DisabledReason = ""
		}
	}
	if target.Kind == db.StandingTargetGroup {
		order.TargetKind = db.StandingTargetGroup
		order.GroupID = target.Group.ID
		order.TargetRole = role
	} else {
		order.TargetKind = db.StandingTargetConv
		order.TargetAgent = target.Agent
	}
	if err := order.Validate(); err != nil {
		writeDashboardStandingOrderError(w, "validate", err)
		return nil, 0, time.Time{}, false
	}
	return order, body.Revision, expectedUpdatedAt, true
}

func dashboardStandingOrderCASFromQuery(
	w http.ResponseWriter, r *http.Request,
) (int64, time.Time, bool) {
	revision, err := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if err != nil || revision <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "revision is required")
		return 0, time.Time{}, false
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("updated_at"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "updated_at is required")
		return 0, time.Time{}, false
	}
	return revision, updatedAt, true
}

func writeDashboardStandingOrder(w http.ResponseWriter, id int64) {
	order, err := db.GetStandingOrder(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "read result: "+err.Error())
		return
	}
	if order == nil {
		writeError(w, http.StatusNotFound, "not_found", "standing order not found")
		return
	}
	view := dashboardStandingOrderView(order, map[int64]string{})
	writeJSON(w, http.StatusOK, view)
}

func writeDashboardStandingOrderError(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, db.ErrStandingOrderInvalid):
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
	case errors.Is(err, db.ErrStandingOrderNameTaken):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, db.ErrStandingOrderRevisionConflict):
		writeError(w, http.StatusConflict, "conflict",
			"standing order changed after this editor opened; reload and try again")
	default:
		writeError(w, http.StatusInternalServerError, "io", operation+": "+err.Error())
	}
}
