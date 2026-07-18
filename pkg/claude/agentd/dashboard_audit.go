package agentd

import (
	"net/http"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// dashboard_audit.go serves the dashboard's Audit tab — the read side of
// the command trail (JOH-268). Like the Messages tab, filtering, sorting
// and pagination all happen server-side (in SQLite) so the tab stays
// responsive no matter how large the trail grows; the client only ever
// holds one page. Fetched on tab activation + filter/sort/page changes,
// not the 2s snapshot poll.

// Audit page-size bounds. defaultAuditPageSize is what the dashboard
// requests when the operator hasn't picked one; maxAuditPageSize caps a
// hand-crafted query so it can't ask the daemon to materialise an
// unbounded page.
const (
	defaultAuditPageSize = 100
	maxAuditPageSize     = 1000
)

// auditEntryView is the JSON shape one audit row takes on the wire. It
// mirrors db.AuditLogEntry but renders At as an RFC3339 string and omits
// empty optional fields.
type auditEntryView struct {
	ID              int64  `json:"id"`
	At              string `json:"at"`
	ActorKind       string `json:"actor_kind"`
	ActorConv       string `json:"actor_conv,omitempty"`
	ActorAgent      string `json:"actor_agent,omitempty"`
	ActorLabel      string `json:"actor_label"`
	Verb            string `json:"verb"`
	TargetConv      string `json:"target_conv,omitempty"`
	TargetAgent     string `json:"target_agent,omitempty"`
	TargetLabel     string `json:"target_label,omitempty"`
	GroupName       string `json:"group_name,omitempty"`
	Detail          string `json:"detail,omitempty"`
	Method          string `json:"method"`
	Path            string `json:"path"`
	Status          int    `json:"status"`
	Source          string `json:"source"`
	EventID         string `json:"event_id,omitempty"`
	RelatedEventID  string `json:"related_event_id,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	TmuxSession     string `json:"tmux_session,omitempty"`
	PaneID          string `json:"pane_id,omitempty"`
	Observer        string `json:"observer,omitempty"`
	CauseKind       string `json:"cause_kind,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	Signal          string `json:"signal,omitempty"`
	LifecycleAction string `json:"lifecycle_action,omitempty"`
	Reason          string `json:"reason,omitempty"`
	ObservedState   string `json:"observed_state,omitempty"`
}

// auditResponse is the Audit tab payload: one page of rows, the pager
// state (page / page_size / totals), the active sort, and the retention
// policy so the UI can show "keeping N days" (or "kept forever").
type auditResponse struct {
	Entries         []auditEntryView `json:"entries"`
	Page            int              `json:"page"`
	PageSize        int              `json:"page_size"`
	Total           int              `json:"total"`            // rows matching the filters
	TotalUnfiltered int              `json:"total_unfiltered"` // all rows
	Sort            string           `json:"sort"`
	Dir             string           `json:"dir"` // "asc" | "desc"
	RetentionDays   int              `json:"retention_days"`
	PruningOn       bool             `json:"pruning_on"`
}

// normalizeAuditSort whitelists the sort key to a db.AuditSort* constant,
// defaulting to time. Keeps caller input off the ORDER BY entirely.
func normalizeAuditSort(s string) string {
	switch s {
	case db.AuditSortActor, db.AuditSortVerb, db.AuditSortTarget, db.AuditSortStatus:
		return s
	default:
		return db.AuditSortTime
	}
}

func auditPageParams(r *http.Request) (page, pageSize int) {
	page = max(atoiOr(r.URL.Query().Get("page"), 1), 1)
	pageSize = atoiOr(r.URL.Query().Get("page_size"), defaultAuditPageSize)
	if pageSize < 1 {
		pageSize = defaultAuditPageSize
	}
	pageSize = min(pageSize, maxAuditPageSize)
	return page, pageSize
}

func handleDashboardAudit(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	sortBy := normalizeAuditSort(q.Get("sort"))
	asc := q.Get("dir") == "asc"
	base := db.AuditLogFilter{
		Verb:    q.Get("verb"),
		Source:  q.Get("source"),
		Outcome: q.Get("outcome"),
		Search:  q.Get("q"),
	}

	total, err := db.CountAuditLog(base)
	if err != nil {
		http.Error(w, "count audit log: "+err.Error(), http.StatusInternalServerError)
		return
	}
	totalUnfiltered, err := db.CountAuditLog(db.AuditLogFilter{})
	if err != nil {
		http.Error(w, "count audit log: "+err.Error(), http.StatusInternalServerError)
		return
	}

	page, pageSize := auditPageParams(r)
	page, offset := clampOffset(page, pageSize, total)

	listFilter := base
	listFilter.SortBy = sortBy
	listFilter.Asc = asc
	listFilter.Limit = pageSize
	listFilter.Offset = offset

	rows, err := db.ListAuditLog(listFilter)
	if err != nil {
		http.Error(w, "list audit log: "+err.Error(), http.StatusInternalServerError)
		return
	}

	entries := make([]auditEntryView, 0, len(rows))
	for _, e := range rows {
		entries = append(entries, auditEntryView{
			ID:              e.ID,
			At:              e.At.Format(time.RFC3339),
			ActorKind:       e.ActorKind,
			ActorConv:       e.ActorConv,
			ActorAgent:      e.ActorAgent,
			ActorLabel:      e.ActorLabel,
			Verb:            e.Verb,
			TargetConv:      e.TargetConv,
			TargetAgent:     e.TargetAgent,
			TargetLabel:     e.TargetLabel,
			GroupName:       e.GroupName,
			Detail:          e.Detail,
			Method:          e.Method,
			Path:            e.Path,
			Status:          e.Status,
			Source:          e.Source,
			EventID:         e.EventID,
			RelatedEventID:  e.RelatedEventID,
			SessionID:       e.SessionID,
			TmuxSession:     e.TmuxSession,
			PaneID:          e.PaneID,
			Observer:        e.Observer,
			CauseKind:       e.CauseKind,
			ExitCode:        e.ExitCode,
			Signal:          e.Signal,
			LifecycleAction: e.LifecycleAction,
			Reason:          e.Reason,
			ObservedState:   e.ObservedState,
		})
	}

	dir := "desc"
	if asc {
		dir = "asc"
	}
	cfg, _ := config.Load()
	days, prune := cfg.ResolvedAuditRetentionDays()
	writeJSON(w, http.StatusOK, auditResponse{
		Entries:         entries,
		Page:            page,
		PageSize:        pageSize,
		Total:           total,
		TotalUnfiltered: totalUnfiltered,
		Sort:            sortBy,
		Dir:             dir,
		RetentionDays:   days,
		PruningOn:       prune,
	})
}
