package agentd

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// dashboard_jobs.go — the Jobs tab's unified job listing.
//
// GET /api/jobs?offset=&limit=&q= returns ONE list merging every job kind the
// dashboard tracks — per-agent export jobs (export.go) and recurring cron
// schedules — discriminated by a `kind` field, newest-activity-first. It
// shares the offset/limit/q contract and response envelope of the /api/retired
// family (dashboard_lists.go): pagination and the text filter are SERVER-side
// (the filter searches the whole set, not the loaded page); column sorting
// stays CLIENT-side over the served window, like every other dashboard table
// (sort.js). Cookie-authed (dashboard-only).
//
// Both sources are merged in Go rather than SQL: they live in different
// tables with disjoint shapes, both are small (cron jobs are hand-made;
// export jobs are TTL-swept after 30 days), and the per-row view mapping
// (labelForConv, group-name resolution) already runs in Go.

// dashboardJobRow is one row of the unified listing. Kind discriminates which
// payload is set: "export" → Export, "cron" → Cron. The payloads reuse the
// existing per-kind views verbatim so the front-end renders each kind with the
// same cell builders (and hands Cron straight to the cron edit modal).
type dashboardJobRow struct {
	Kind   string              `json:"kind"`
	Export *dashboardExportJob `json:"export,omitempty"`
	Cron   *dashboardCronJob   `json:"cron,omitempty"`
}

const (
	jobKindExport = "export"
	jobKindCron   = "cron"
)

// handleDashboardJobs serves GET /api/jobs — the Jobs tab's unified window.
func handleDashboardJobs(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	offset, limit, q := listPageParams(r)

	rows := collectJobRows()
	totalUnfiltered := len(rows)
	if q != "" {
		filtered := rows[:0]
		for _, row := range rows {
			if jobRowMatches(row, q) {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	total := len(rows)

	served := clampListOffset(offset, limit, total)
	window := rows[served:]
	if limit > 0 && len(window) > limit {
		window = window[:limit]
	}
	writeListPage(w, window, served, limit, total, totalUnfiltered)
}

// collectJobRows builds the full unified list, newest-activity-first — the
// default ordering a sort-less table falls back to. "Activity" is the export's
// creation (an in-flight export is always fresh) and the cron job's last run
// (falling back to its creation for never-run jobs). Best-effort per source: a
// failed read yields that source absent rather than a failed endpoint.
func collectJobRows() []dashboardJobRow {
	type keyed struct {
		row dashboardJobRow
		at  time.Time
	}
	var all []keyed

	if exports, err := db.ListExportJobs(0); err == nil {
		for _, j := range exports {
			view := dashboardExportJob{
				exportJobView: exportJobToView(j),
				ConvLabel:     labelForConv(j.ConvID),
			}
			all = append(all, keyed{
				row: dashboardJobRow{Kind: jobKindExport, Export: &view},
				at:  j.CreatedAt,
			})
		}
	}
	if crons, err := db.ListAgentCronJobs(); err == nil {
		groupNames := map[int64]string{}
		for _, j := range crons {
			view := cronJobToView(j, groupNames)
			at := j.LastRunAt
			if at.IsZero() {
				at = j.CreatedAt
			}
			all = append(all, keyed{
				row: dashboardJobRow{Kind: jobKindCron, Cron: &view},
				at:  at,
			})
		}
	}

	// Sort on the real timestamps, NOT their JSON strings — the export views
	// carry RFC3339Nano, which is not reliably lexically ordered. Ties (and
	// zero times) break on kind+id so pagination windows stay stable.
	sort.SliceStable(all, func(a, b int) bool {
		if !all[a].at.Equal(all[b].at) {
			return all[a].at.After(all[b].at)
		}
		if all[a].row.Kind != all[b].row.Kind {
			return all[a].row.Kind < all[b].row.Kind
		}
		return jobRowID(all[a].row) > jobRowID(all[b].row)
	})

	out := make([]dashboardJobRow, 0, len(all))
	for _, k := range all {
		out = append(out, k.row)
	}
	return out
}

func jobRowID(r dashboardJobRow) int64 {
	if r.Export != nil {
		return r.Export.ID
	}
	if r.Cron != nil {
		return r.Cron.ID
	}
	return 0
}

// jobRowMatches is the server-side text filter: a case-insensitive contains
// over each kind's human-searchable fields (mirrors what the old client-side
// cron filter matched, plus the export fields).
func jobRowMatches(r dashboardJobRow, q string) bool {
	needle := strings.ToLower(q)
	var hay []string
	switch {
	case r.Export != nil:
		e := r.Export
		hay = []string{jobKindExport, e.Title, e.Status, e.ConvLabel, e.ConvID,
			e.ArtifactName, e.Error, e.Preset}
	case r.Cron != nil:
		c := r.Cron
		hay = []string{jobKindCron, c.Name, c.Subject, c.Body,
			c.OwnerLabel, c.OwnerAgent, c.OwnerConv,
			c.TargetLabel, c.TargetAgent, c.TargetConv,
			c.GroupName, c.LastRunStatus}
	}
	for _, h := range hay {
		if h != "" && strings.Contains(strings.ToLower(h), needle) {
			return true
		}
	}
	return false
}
