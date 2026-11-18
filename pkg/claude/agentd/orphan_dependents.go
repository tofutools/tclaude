package agentd

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/doctor"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

// Orphaned dependents are what a retirement or deletion left behind before
// the cascade existed, or when it was told to keep them: cron jobs, standing
// orders and automations of a retired or deleted agent, those addressed to
// it, and unfinished runs scoped to it. `tclaude agent cleanup-dependents
// --orphaned` removes them the way a retire's cascade does today, and the
// Doctor counts them.

// orphanedAgentDependents is one retired or deleted agent and what still
// refers to it; after a cleanup, Result is what the cleanup did.
type orphanedAgentDependents struct {
	db.OrphanedAgent
	Title       string              `json:"title,omitempty"`
	Dependents  []db.AgentDependent `json:"dependents,omitempty"`
	Runs        []db.AgentScopedRun `json:"runs,omitempty"`
	PinnedSteps []agentPinnedStep   `json:"pinned_steps,omitempty"`
	Result      *agentCascade       `json:"result,omitempty"`
	Error       string              `json:"error,omitempty"`
}

// orphanAssigneeMatcher matches an assignee naming agentID by its stable id
// or one of its conversations, never by name: a retired agent's name may
// since have gone to another agent, so a step pinned by name is left as
// written.
func orphanAssigneeMatcher(agentID string) func(string) bool {
	convs, _ := db.ConvsForAgent(agentID)
	ids := map[string]bool{agentID: true}
	for _, conv := range convs {
		ids[conv] = true
	}
	return func(assignee string) bool { return ids[strings.TrimSpace(assignee)] }
}

// listOrphanedAgentDependents finds the orphans. withSteps also walks the
// process templates for steps pinned to each one, which the Doctor skips.
func listOrphanedAgentDependents(ctx context.Context, withSteps bool) ([]orphanedAgentDependents, []string, error) {
	agents, err := db.ListOrphanedAgents()
	if err != nil {
		return nil, nil, err
	}
	var unavailable []string
	var fs *store.FS
	if withSteps && len(agents) > 0 {
		if fs, err = store.NewFS(processStoreRoot()); err != nil {
			unavailable = append(unavailable, "process templates: "+err.Error())
			fs = nil
		}
	}
	out := make([]orphanedAgentDependents, 0, len(agents))
	for _, a := range agents {
		o := orphanedAgentDependents{OrphanedAgent: a}
		if a.ConvID != "" && withSteps {
			if title := strings.TrimSpace(agent.CachedTitle(a.ConvID)); title != agent.UnknownTitle {
				o.Title = title
			}
		}
		if o.Dependents, err = db.ListAgentDependents(a.AgentID); err != nil {
			return nil, nil, err
		}
		if o.Runs, err = db.ListAgentScopedActiveRuns(a.AgentID); err != nil {
			return nil, nil, err
		}
		if fs != nil {
			steps, err := templatesPinnedTo(ctx, fs, orphanAssigneeMatcher(a.AgentID), nil)
			if err != nil {
				unavailable = append(unavailable, "process templates: "+err.Error())
				fs = nil
			}
			o.PinnedSteps = steps
		}
		out = append(out, o)
	}
	return out, unavailable, nil
}

// cleanupOrphanedAgentDependents removes or repairs what each orphan still
// has, as a retire's cascade would, attributed to by. An agent reinstated
// before its rows are removed is left as it is. One reinstated in the moment
// between that and the run cancellations still has its scoped runs cancelled
// and its pinned steps cleared; the window is one agent's cascade long, and
// closing it would mean holding the reinstate out for the template walk.
func cleanupOrphanedAgentDependents(ctx context.Context, by string) ([]orphanedAgentDependents, error) {
	orphans, _, err := listOrphanedAgentDependents(ctx, false)
	if err != nil {
		return nil, err
	}
	for i := range orphans {
		o := &orphans[i]
		matches := orphanAssigneeMatcher(o.AgentID)
		cronAuthorityMu.Lock()
		removed, orphaned, err := db.RemoveOrphanedAgentDependents(o.AgentID)
		cronAuthorityMu.Unlock()
		switch {
		case err != nil:
			o.Error = err.Error()
			continue
		case !orphaned:
			o.Error = "reinstated meanwhile; left as it is"
			continue
		}
		res := finishAgentCascade(ctx, o.AgentID, by, o.State, false, removed, matches)
		o.Result = &res
	}
	return orphans, nil
}

// handleOrphanedDependents serves /v1/orphaned-dependents: GET lists the
// orphans and what a cleanup would do, POST does it. Operator only: the
// cleanup removes other agents' cron jobs, standing orders and automations,
// and standing orders have no agent permission.
func handleOrphanedDependents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if !requireHuman(w, r, "list orphaned dependents") {
			return
		}
		orphans, unavailable, err := listOrphanedAgentDependents(r.Context(), true)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"agents": orphans, "unavailable": unavailable})
	case http.MethodPost:
		if !requireHuman(w, r, "clean up orphaned dependents") {
			return
		}
		orphans, err := cleanupOrphanedAgentDependents(r.Context(), "human")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		invalidateDoctorChecks()
		writeJSON(w, http.StatusOK, map[string]any{"agents": orphans})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

// orphanedDependentsChecks is the Doctor's count of orphans. It reads rows
// only; the template walk is left to the command's preview.
func orphanedDependentsChecks() []doctor.Check {
	orphans, _, err := listOrphanedAgentDependents(context.Background(), false)
	if err != nil || len(orphans) == 0 {
		return nil
	}
	rows, runs := 0, 0
	for _, o := range orphans {
		rows += len(o.Dependents)
		runs += len(o.Runs)
	}
	var parts []string
	if rows > 0 {
		parts = append(parts, fmt.Sprintf("%d cron job(s), standing order(s) or automation(s)", rows))
	}
	if runs > 0 {
		parts = append(parts, fmt.Sprintf("%d unfinished run(s)", runs))
	}
	return []doctor.Check{{
		ID: "orphaned-dependents", Category: doctor.CategorySetup, Severity: doctor.SeverityInfo,
		Title: fmt.Sprintf("%s still refer to %d retired or deleted agent(s)", strings.Join(parts, " and "), len(orphans)),
		Detail: "Left behind by retirements and deletions before they took what refers to the agent, or ones told to " +
			"keep it. A retired agent's own cron jobs and standing orders sit disabled; the rest fire at nobody or wait " +
			"for a performer that is gone. Preview with --dry-run, then remove them.",
		Fix: "tclaude agent cleanup-dependents --orphaned",
	}}
}
