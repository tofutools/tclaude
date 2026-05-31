package agentd

import (
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/ccworkflows"
	"github.com/tofutools/tclaude/pkg/claude/common/convindex"
)

// Cookie-authed, read-only /api/workflows endpoints backing the dashboard's
// "Workflows" tab. They read Claude Code's builtin workflow runs + saved
// templates through the ccworkflows data layer and join each run's launching
// session (RunRef.SessionID) to tclaude's conv_index — the cross-agent
// "who is using workflows" view that is the point of phase 2.
//
// v1 is read-only and poll-driven (live tailing is a later slice). The wire
// shapes below are the stable contract the mermaid follow-up and the live slice
// build on, so they are explicit view structs, not the raw ccworkflows types.
//
//	GET /api/workflows            → { saved: [...], runs: [...] }   (list)
//	GET /api/workflows/{runId}    → run detail: phase/agent tree + script
func registerDashboardWorkflowsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/workflows", dashboardReadRoute(handleDashboardWorkflowsList))
	mux.HandleFunc("GET /api/workflows/{runId}", dashboardReadRoute(handleDashboardWorkflowDetail))
}

// dashboardReadRoute adapts a plain handler into a cookie-authed read route:
// it runs the dashboard cookie/Origin auth, then calls the handler. (Read-only,
// so unlike dashboardTemplatesRoute it needs no synthetic human peer — there is
// no permission slug to check on a GET.)
func dashboardReadRoute(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		fn(w, r)
	}
}

// workflowConvJoin is the launching session's identity, joined from conv_index.
// Every field is best-effort: a run whose session is not (yet) indexed still
// lists, just without a title/cwd.
type workflowConvJoin struct {
	SessionID  string `json:"sessionId"`
	ProjectDir string `json:"projectDir,omitempty"`
	ConvTitle  string `json:"convTitle,omitempty"`
	ConvCwd    string `json:"convCwd,omitempty"`
	GitBranch  string `json:"gitBranch,omitempty"`
}

// workflowRunView is the per-run row in the list response: the lightweight
// ccworkflows.RunRef fields plus the conv join.
type workflowRunView struct {
	RunID            string `json:"runId"`
	WorkflowName     string `json:"workflowName,omitempty"`
	Status           string `json:"status"`
	StartTimeMs      int64  `json:"startTimeMs,omitempty"`
	AgentCount       int    `json:"agentCount,omitempty"`
	HasCompletedJSON bool   `json:"hasCompletedJson"`
	workflowConvJoin
}

// workflowsListView is the GET /api/workflows response.
type workflowsListView struct {
	Saved []ccworkflows.SavedScript `json:"saved"`
	Runs  []workflowRunView         `json:"runs"`
}

// workflowDetailView is the GET /api/workflows/{runId} response: the full typed
// run state (phase/agent tree + script, flattened) plus the conv join and the
// mermaid projection. Mermaid is generated server-side from the SAME RunState
// (via ccworkflows.Mermaid) so the dashboard never re-derives the graph — one
// source of truth shared with the CLI's `workflows show --mermaid`.
type workflowDetailView struct {
	*ccworkflows.RunState
	Join    workflowConvJoin `json:"join"`
	Mermaid string           `json:"mermaid,omitempty"`
}

// joinConv resolves a session id to its conv_index identity (best-effort).
func joinConv(sessionID, projectDir string) workflowConvJoin {
	j := workflowConvJoin{SessionID: sessionID, ProjectDir: projectDir}
	row := agent.FreshConvRowResolved(sessionID)
	if row == nil {
		return j
	}
	j.ConvTitle = convindex.FormatConvTitle(row.CustomTitle, row.Summary, row.FirstPrompt)
	j.ConvCwd = row.ProjectPath
	j.GitBranch = row.GitBranch
	return j
}

func handleDashboardWorkflowsList(w http.ResponseWriter, _ *http.Request) {
	saved, err := ccworkflows.DefaultSavedScripts("") // global user templates
	if err != nil {
		// A missing saved/ dir is not an error; only a real read failure is.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	refs, err := ccworkflows.ListAllRuns()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	out := workflowsListView{Saved: saved, Runs: make([]workflowRunView, 0, len(refs))}
	for _, r := range refs {
		out.Runs = append(out.Runs, workflowRunView{
			RunID:            r.RunID,
			WorkflowName:     r.WorkflowName,
			Status:           string(r.Status),
			StartTimeMs:      r.StartTimeMs,
			AgentCount:       r.AgentCount,
			HasCompletedJSON: r.HasCompletedJSON,
			workflowConvJoin: joinConv(r.SessionID, r.ProjectDir),
		})
	}
	if out.Saved == nil {
		out.Saved = []ccworkflows.SavedScript{}
	}
	writeJSON(w, http.StatusOK, out)
}

func handleDashboardWorkflowDetail(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("runId")
	if runID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing run id"})
		return
	}
	rs, ref, err := ccworkflows.FindRun(runID)
	if err != nil {
		// FindRun returns a non-nil ref when the run was located but failed to
		// load (corrupt JSON / unparseable journal) — that's a 500, not a 404.
		status := http.StatusNotFound
		if ref != nil {
			status = http.StatusInternalServerError
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	// On the success path FindRun always returns a non-nil ref.
	writeJSON(w, http.StatusOK, workflowDetailView{
		RunState: rs,
		Join:     joinConv(ref.SessionID, ref.ProjectDir),
		Mermaid:  ccworkflows.Mermaid(rs),
	})
}
