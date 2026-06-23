package agentd

import (
	"net/http"
	"strconv"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// dashboard_audit.go serves the dashboard's Audit tab — the read side of
// the command trail (JOH-268). Like the Costs tab it is fetched on tab
// activation rather than riding the 2s snapshot poll, so the append-only
// log can grow without bloating every refresh.

// auditEntryView is the JSON shape one audit row takes on the wire. It
// mirrors db.AuditLogEntry but renders At as an RFC3339 string and omits
// empty optional fields.
type auditEntryView struct {
	ID          int64  `json:"id"`
	At          string `json:"at"`
	ActorKind   string `json:"actor_kind"`
	ActorConv   string `json:"actor_conv,omitempty"`
	ActorLabel  string `json:"actor_label"`
	Verb        string `json:"verb"`
	TargetConv  string `json:"target_conv,omitempty"`
	TargetLabel string `json:"target_label,omitempty"`
	GroupName   string `json:"group_name,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Status      int    `json:"status"`
	Source      string `json:"source"`
}

// auditResponse is the Audit tab payload: the rows plus the retention
// policy so the UI can show "keeping N days" (or "kept forever").
type auditResponse struct {
	Entries       []auditEntryView `json:"entries"`
	RetentionDays int              `json:"retention_days"`
	PruningOn     bool             `json:"pruning_on"`
}

// auditTabMaxLimit caps how many rows one Audit-tab fetch returns, so a
// long-lived trail can't ship an unbounded payload to the browser. The
// UI's text/verb/actor filters narrow this client-side.
const auditTabMaxLimit = 2000

func handleDashboardAudit(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query()
	filter := db.AuditLogFilter{
		Verb:    q.Get("verb"),
		Source:  q.Get("source"),
		Outcome: q.Get("outcome"),
		Limit:   auditTabMaxLimit,
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < auditTabMaxLimit {
			filter.Limit = n
		}
	}

	rows, err := db.ListAuditLog(filter)
	if err != nil {
		http.Error(w, "list audit log: "+err.Error(), http.StatusInternalServerError)
		return
	}

	entries := make([]auditEntryView, 0, len(rows))
	for _, e := range rows {
		entries = append(entries, auditEntryView{
			ID:          e.ID,
			At:          e.At.Format(time.RFC3339),
			ActorKind:   e.ActorKind,
			ActorConv:   e.ActorConv,
			ActorLabel:  e.ActorLabel,
			Verb:        e.Verb,
			TargetConv:  e.TargetConv,
			TargetLabel: e.TargetLabel,
			GroupName:   e.GroupName,
			Detail:      e.Detail,
			Method:      e.Method,
			Path:        e.Path,
			Status:      e.Status,
			Source:      e.Source,
		})
	}

	cfg, _ := config.Load()
	days, prune := cfg.ResolvedAuditRetentionDays()
	writeJSON(w, http.StatusOK, auditResponse{
		Entries:       entries,
		RetentionDays: days,
		PruningOn:     prune,
	})
}
