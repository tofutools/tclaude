package agentd

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Dashboard cron mutation routes — sibling of the /v1/cron/* surface
// but cookie-authenticated for the browser. The dashboard is a human
// surface, so we don't apply the per-job visibility filter the /v1/
// endpoints use; the human can act on any row.
//
// Wired into the popup mux from registerDashboardEditRoutes.

func registerDashboardCronRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/cron", handleDashboardCronCreate)
	mux.HandleFunc("/api/cron/", handleDashboardCronAPI)
}

// handleDashboardCronAPI dispatches:
//
//	GET    /api/cron/{id}/logs[?limit=N]   → recent run history
//	POST   /api/cron                       → create a new cron job
//	POST   /api/cron/{id}/enable           → enable
//	POST   /api/cron/{id}/disable          → disable
//	POST   /api/cron/{id}/run-now          → fire + stamp last_run
//	PATCH  /api/cron/{id}                  → partial update
//	DELETE /api/cron/{id}                  → delete
func handleDashboardCronAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/cron/")
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" {
		http.Error(w, "expected /api/cron/{id}", http.StatusNotFound)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "id must be an integer", http.StatusBadRequest)
		return
	}
	job, err := db.GetAgentCronJob(id)
	if err != nil {
		http.Error(w, "lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if job == nil {
		http.Error(w, "job "+strconv.FormatInt(id, 10)+" not found", http.StatusNotFound)
		return
	}
	if len(parts) == 2 {
		switch parts[1] {
		case "logs":
			if r.Method != http.MethodGet {
				http.Error(w, "GET only", http.StatusMethodNotAllowed)
				return
			}
			dashboardCronLogs(w, r, id)
		case "enable":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			if err := db.SetAgentCronJobEnabled(id, true); err != nil {
				http.Error(w, "update: "+err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "disable":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			if err := db.SetAgentCronJobEnabled(id, false); err != nil {
				http.Error(w, "update: "+err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "run-now":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			now := time.Now()
			status := fireCronJob(job, now)
			if err := db.UpdateAgentCronJobLastRun(id, now, status); err != nil {
				http.Error(w, "stamp: "+err.Error(), http.StatusInternalServerError)
				return
			}
			_, _ = db.InsertAgentCronRun(&db.AgentCronRun{
				JobID:   id,
				FiredAt: now,
				Status:  status,
			})
			writeJSON(w, http.StatusOK, map[string]any{"status": status})
		default:
			http.Error(w, "unknown subpath /api/cron/{id}/"+parts[1], http.StatusNotFound)
		}
		return
	}
	switch r.Method {
	case http.MethodDelete:
		if err := db.DeleteAgentCronJob(id); err != nil {
			http.Error(w, "delete: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPatch:
		dashboardCronPatch(w, r, id)
	default:
		http.Error(w, "DELETE or PATCH on /api/cron/{id}", http.StatusMethodNotAllowed)
	}
}

// handleDashboardCronCreate is the cookie-auth twin of POST /v1/cron.
// Delegates to handleCronCreate after stamping a synthetic human peer
// on the request — the cookie+Origin pin is the human-consent layer,
// and the inner handler's authCronWrite then sees a classHuman caller
// (asDashboardHumanPeer sets DashboardHuman).
func handleDashboardCronCreate(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	handleCronCreate(w, asDashboardHumanPeer(r))
}

// dashboardCronPatch is the cookie-auth twin of PATCH /v1/cron/{id}.
// Same body shape, validation, and last_run_at-preservation rule as
// the /v1 handler. Stamps a human peer so the inner authCronWrite
// passes without a slug check.
func dashboardCronPatch(w http.ResponseWriter, r *http.Request, id int64) {
	handleCronPatch(w, asDashboardHumanPeer(r), id)
}

// dashboardCronLogs returns the recent run history for one job. Same
// shape as the /v1/cron/{id}/logs response so the dashboard can reuse
// the same parsing helpers (if we ever share JS).
func dashboardCronLogs(w http.ResponseWriter, r *http.Request, id int64) {
	limit := 25
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	runs, err := db.ListAgentCronRunsForJob(id, limit)
	if err != nil {
		http.Error(w, "list runs: "+err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		out = append(out, map[string]any{
			"id":        run.ID,
			"fired_at":  run.FiredAt.Format(time.RFC3339),
			"status":    run.Status,
			"error_msg": run.ErrorMsg,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runs": out,
	})
}
