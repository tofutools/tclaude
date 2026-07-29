package agentd

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/hookevents"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Dashboard standing-order mutation routes. This is intentionally a
// dashboard-only, human-authorized surface: agent-side proposals and
// permission-gated activation remain a separate workflow.
func registerDashboardStandingOrderRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/standing-orders", handleDashboardStandingOrderCreate)
	mux.HandleFunc("/api/standing-orders/", handleDashboardStandingOrderAPI)
	mux.HandleFunc("/api/standing-order-hooks", handleDashboardStandingOrderHooks)
}

type dashboardStandingOrderMutation struct {
	Name       string `json:"name"`
	RowVersion int64  `json:"row_version,omitempty"`
	// Revision and UpdatedAt are accepted from a dashboard tab opened before
	// the row-version migration. New clients use RowVersion exclusively.
	Revision        int64                 `json:"revision,omitempty"`
	UpdatedAt       string                `json:"updated_at,omitempty"`
	Target          string                `json:"target"`
	Role            string                `json:"role,omitempty"`
	Summary         string                `json:"summary"`
	Sources         []string              `json:"sources,omitempty"`
	Timing          string                `json:"timing"`
	Cadence         string                `json:"cadence"`
	CooldownSeconds int64                 `json:"cooldown_seconds,omitempty"`
	DebounceSeconds int64                 `json:"debounce_seconds,omitempty"`
	Enabled         *bool                 `json:"enabled,omitempty"`
	TriggerEvent    string                `json:"trigger_event,omitempty"`
	HookSelectors   []hookevents.Selector `json:"hook_selectors,omitempty"`
	MatchField      string                `json:"match_field,omitempty"`
	MatchRegex      string                `json:"match_regex,omitempty"`
}

func handleDashboardStandingOrderCreate(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST")
		return
	}
	order, _, ok := decodeDashboardStandingOrder(w, r, nil)
	if !ok {
		return
	}
	id, err := db.InsertStandingOrder(order)
	if err != nil {
		writeDashboardStandingOrderError(w, "create", err)
		return
	}
	warning := reconcileStandingOrderHookDeclarations(nil, order)
	writeDashboardStandingOrder(w, id, warning)
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
		rowVersion, ok := dashboardStandingOrderCASFromQuery(w, r, order)
		if !ok {
			return
		}
		if err := db.SetStandingOrderEnabled(id, enabled, rowVersion); err != nil {
			writeDashboardStandingOrderError(w, "toggle", err)
			return
		}
		updated, _ := db.GetStandingOrder(id)
		if warning := reconcileStandingOrderHookDeclarations(order, updated); warning != "" {
			w.Header().Set("X-Tclaude-Hook-Warning", warning)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		replacement, rowVersion, ok := decodeDashboardStandingOrder(w, r, order)
		if !ok {
			return
		}
		if err := db.UpdateStandingOrder(id, rowVersion, replacement); err != nil {
			writeDashboardStandingOrderError(w, "update", err)
			return
		}
		warning := reconcileStandingOrderHookDeclarations(order, replacement)
		writeDashboardStandingOrder(w, id, warning)
	case http.MethodDelete:
		rowVersion, ok := dashboardStandingOrderCASFromQuery(w, r, order)
		if !ok {
			return
		}
		if err := db.DeleteStandingOrder(id, rowVersion); err != nil {
			writeDashboardStandingOrderError(w, "delete", err)
			return
		}
		if warning := reconcileStandingOrderHookDeclarations(order, nil); warning != "" {
			w.Header().Set("X-Tclaude-Hook-Warning", warning)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "PATCH or DELETE")
	}
}

func decodeDashboardStandingOrder(
	w http.ResponseWriter, r *http.Request, existing *db.StandingOrder,
) (*db.StandingOrder, int64, bool) {
	creating := existing == nil
	var body dashboardStandingOrderMutation
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "bad body: "+err.Error())
		return nil, 0, false
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "name is required")
		return nil, 0, false
	}
	if err := validateCronName(body.Name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return nil, 0, false
	}
	expectedRowVersion := int64(0)
	if !creating {
		var ok bool
		expectedRowVersion, ok = dashboardStandingOrderCAS(
			w, body.RowVersion, body.Revision, body.UpdatedAt, existing)
		if !ok {
			return nil, 0, false
		}
	}
	body.Target = strings.TrimSpace(body.Target)
	if body.Target == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "target is required")
		return nil, 0, false
	}
	var target cronTarget
	if strings.EqualFold(body.Target, db.StandingTargetGlobal) {
		target.Kind = db.StandingTargetGlobal
	} else {
		var err error
		target, err = resolveCronTarget(body.Target)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "resolve target: "+err.Error())
			return nil, 0, false
		}
	}
	if target.Kind != db.StandingTargetGroup &&
		target.Kind != db.StandingTargetGlobal &&
		target.Agent == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"target must resolve to an enrolled agent with a stable agt_ id")
		return nil, 0, false
	}
	role := strings.TrimSpace(body.Role)
	if strings.EqualFold(role, "all") {
		role = ""
	}
	if role != "" && target.Kind != db.StandingTargetGroup {
		writeError(w, http.StatusBadRequest, "invalid_arg", "role is only valid for a group target")
		return nil, 0, false
	}
	trigger := strings.TrimSpace(body.TriggerEvent)
	selectors := hookevents.NormalizeSelectors(body.HookSelectors)
	if len(selectors) > 0 {
		trigger = db.StandingTriggerHookEvent
	}
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
		HookSelectors:    selectors,
		TriggerSources:   body.Sources,
		MatchField:       body.MatchField,
		MatchRegex:       body.MatchRegex,
		Timing:           body.Timing,
		Cadence:          body.Cadence,
		CooldownSeconds:  body.CooldownSeconds,
		DebounceSeconds:  body.DebounceSeconds,
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
	switch target.Kind {
	case db.StandingTargetGroup:
		order.TargetKind = db.StandingTargetGroup
		order.GroupID = target.Group.ID
		order.TargetRole = role
	case db.StandingTargetGlobal:
		order.TargetKind = db.StandingTargetGlobal
	default:
		order.TargetKind = db.StandingTargetConv
		order.TargetAgent = target.Agent
	}
	if err := order.Validate(); err != nil {
		writeDashboardStandingOrderError(w, "validate", err)
		return nil, 0, false
	}
	return order, expectedRowVersion, true
}

func dashboardStandingOrderCASFromQuery(
	w http.ResponseWriter, r *http.Request, current *db.StandingOrder,
) (int64, bool) {
	query := r.URL.Query()
	rowVersion, _ := strconv.ParseInt(query.Get("row_version"), 10, 64)
	revision, _ := strconv.ParseInt(query.Get("revision"), 10, 64)
	return dashboardStandingOrderCAS(
		w, rowVersion, revision, query.Get("updated_at"), current)
}

func dashboardStandingOrderCAS(
	w http.ResponseWriter,
	rowVersion, legacyRevision int64,
	legacyUpdatedAt string,
	current *db.StandingOrder,
) (int64, bool) {
	if rowVersion > 0 {
		return rowVersion, true
	}
	if legacyRevision <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "row_version is required")
		return 0, false
	}
	if strings.TrimSpace(legacyUpdatedAt) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"legacy updated_at is required with revision")
		return 0, false
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, legacyUpdatedAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "legacy updated_at is invalid")
		return 0, false
	}
	// A dashboard tab opened before row_version existed carries the old
	// two-part token. Both halves must still match: lifecycle writers used to
	// advance updated_at without revision, so accepting revision alone could
	// let a stale tab undo an automatic retirement. Translate only a CURRENT
	// legacy token to row_version; the database CAS still closes a mutation
	// that races after this read.
	if current == nil ||
		current.Revision != legacyRevision ||
		!current.UpdatedAt.Equal(updatedAt) {
		writeDashboardStandingOrderError(
			w, "legacy concurrency check", db.ErrStandingOrderVersionConflict)
		return 0, false
	}
	return current.RowVersion, true
}

func writeDashboardStandingOrder(w http.ResponseWriter, id int64, warning ...string) {
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
	if len(warning) > 0 && warning[0] != "" {
		w.Header().Set("X-Tclaude-Hook-Warning", warning[0])
		view.HookSetupWarning = warning[0]
	}
	writeJSON(w, http.StatusOK, view)
}

func handleDashboardStandingOrderHooks(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hooks": hookevents.All()})
}

func reconcileStandingOrderHookDeclarations(
	before, after *db.StandingOrder,
) string {
	harnesses := map[string]bool{}
	collect := func(order *db.StandingOrder, active bool) {
		if order == nil {
			return
		}
		for _, selector := range order.HookSelectors {
			if active {
				harnesses[selector.Harness] = true
			}
		}
	}
	// The old harness must be reconciled even when the old row was disabled:
	// this is also the cleanup path for declarations left by an interrupted
	// earlier mutation. The replacement contributes only while it is enabled.
	collect(before, before != nil)
	collect(after, after != nil && after.Enabled)
	return reconcileStandingOrderHookHarnesses(harnesses)
}

func reconcileStandingOrderHookHarnesses(harnesses map[string]bool) string {
	var problems []string
	if harnesses[hookevents.HarnessClaude] {
		if err := session.InstallHooks(); err != nil {
			problems = append(problems, "Claude hook registration failed: "+err.Error())
		}
	}
	if harnesses[hookevents.HarnessCodex] {
		codex, ok := harness.Get(harness.CodexName)
		if !ok || codex.Hooks == nil {
			problems = append(problems, "Codex hook registration is unavailable")
		} else if err := codex.Hooks.Install(); err != nil {
			problems = append(problems, "Codex hook registration failed: "+err.Error())
		} else if trusted, ok := codex.Hooks.(harness.TrustedHookInstaller); ok && !trusted.Trusted() {
			// Dashboard authoring is allowed to declare the selected callback,
			// but execution trust remains the explicit setup boundary. Surface
			// that boundary on the mutation instead of reporting a healthy
			// automation whose newly selected event cannot run.
			problems = append(problems, trusted.TrustNote())
		}
	}
	return strings.Join(problems, "; ")
}

// standingOrderHookHarnessesForGroup snapshots the declaration files a group
// lifecycle mutation may affect. It must run before deletion/disable so rows
// that are about to disappear still identify the files that need pruning.
func standingOrderHookHarnessesForGroup(groupID int64) (map[string]bool, error) {
	orders, err := db.ListStandingOrders()
	if err != nil {
		return nil, err
	}
	harnesses := map[string]bool{}
	for _, order := range orders {
		touchesGroup := order.IsGroupTarget() && order.GroupID == groupID
		if !touchesGroup {
			for _, additionalID := range order.AdditionalGroupIDs {
				if additionalID == groupID {
					touchesGroup = true
					break
				}
			}
		}
		if !touchesGroup {
			continue
		}
		for _, selector := range order.HookSelectors {
			harnesses[selector.Harness] = true
		}
	}
	return harnesses, nil
}

func standingOrderHookHarnessesForGroupBestEffort(groupID int64) map[string]bool {
	harnesses, err := standingOrderHookHarnessesForGroup(groupID)
	if err == nil {
		return harnesses
	}
	// Do not touch either harness's configuration unless a selector proves it
	// is in scope. A later setup/mutation can self-heal an optional declaration;
	// unexpectedly rewriting hook files for an installation that has never
	// authored a native standing order would violate the feature's opt-in seam.
	slog.Warn("standing orders: could not resolve group hook declarations",
		"group_id", groupID, "error", err)
	return map[string]bool{}
}

func writeDashboardStandingOrderError(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, db.ErrStandingOrderInvalid):
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
	case errors.Is(err, db.ErrStandingOrderNameTaken):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, db.ErrStandingOrderVersionConflict):
		writeError(w, http.StatusConflict, "conflict",
			"standing order changed after this editor opened; reload and try again")
	default:
		writeError(w, http.StatusInternalServerError, "io", operation+": "+err.Error())
	}
}
