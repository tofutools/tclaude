package agentd

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Stand down a task force (JOH-345) — the mirror of `task-force deploy`.
//
// Deploy is one command: it creates a group, spawns the roster in staged
// waves, materializes the template's rhythms as group-target cron jobs, seeds
// process state, and delivers a work pattern. Winding that down used to be
// asymmetric — `groups retire` demotes the members but leaves the seeded
// rhythms firing forever and any pending wave choreography half-run. Stand-down
// is the composed mirror: it retires every member AND sweeps the deploy-seeded
// runtime (the rhythm cron jobs + pending waves), while KEEPING the group row
// as a dormant record so the mission / provenance / process history survive.
// It is deliberately NOT a group delete (`groups rm` already does that) —
// stand-down winds a force down without erasing it.
//
// Composed from existing primitives, no new machinery:
//   - bulkRetireGroupMembers (the shared retire core) demotes the roster;
//   - db.DeleteGroupTargetCronJobs sweeps the seeded rhythms (delete, not the
//     retire path's non-destructive disable — the force is being wound down);
//   - db.DeleteWaveChoreography cancels any pending staged-spawn waves.
//
// Wire surface (group-scoped, under the existing /v1/groups/{name} family):
//
//	POST /v1/groups/{name}/stand-down → retire members + sweep the seeded runtime
//
// Gating: requireGroupPermission(groups.retire) — the SAME bar as retire (the
// human always passes, a group owner passes structurally, else the groups.retire
// slug). Stand-down of ANY group is fine — it is retire + sweep, and a plain
// group simply has no rhythms/waves to sweep — so it is not over-gated on
// "is a deployed force".

// handleGroupStandDown serves POST /v1/groups/{name}/stand-down: retire every
// member of g, then sweep the deploy-seeded group-target cron jobs and any
// pending wave choreography. The group row (mission / source template / process
// history) is deliberately preserved. Shares the retire query knobs
// (?shutdown=, ?reason=): stand-down soft-exits the panes by default (winding
// the force down), and delete_worktree stays available for a full teardown.
func handleGroupStandDown(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST")
		return
	}
	caller, ok := requireGroupPermission(w, r, PermGroupsRetire, g)
	if !ok {
		return
	}

	// Retire the whole roster — no status filter, no explicit selection: a
	// stand-down winds down the entire force. Reuses the shared retire core, so
	// the caller-skip (never self-retire), ownerless warnings, and optional
	// worktree cleanup all behave exactly as a `groups retire` would.
	out, err := bulkRetireGroupMembers(g, caller,
		standDownReason(r), retireShouldShutdown(r), retireShouldDeleteWorktree(r), nil, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}

	// Sweep the deploy-seeded runtime. Best-effort per sweep — a failure is
	// logged, not fatal: the members are already retired, and the leftover
	// rows self-heal (a rhythm fire at a memberless group no-ops; the wave
	// runner drops a row for a group it can't progress). Report what was swept.
	swept := standDownSweep(g)

	resp := standDownResp{
		Group:          out.Group,
		Action:         "stand-down",
		Members:        out.Members,
		RhythmsRemoved: swept.rhythms,
		WavesCancelled: swept.waves,
		Warnings:       out.Warnings,
	}
	writeJSON(w, http.StatusOK, resp)
}

// standDownResp is the stand-down wire shape — the retire member table plus the
// sweep counts. Mirrors groupRetireResp with the two extra fields the CLI /
// dashboard render as "N rhythm job(s) removed, M pending wave(s) cancelled".
type standDownResp struct {
	Group          string           `json:"group"`
	Action         string           `json:"action"`
	Members        []memberOpResult `json:"members"`
	RhythmsRemoved int              `json:"rhythms_removed"`
	WavesCancelled int              `json:"waves_cancelled"`
	Warnings       []string         `json:"warnings,omitempty"`
}

// standDownSweptCounts carries what a stand-down sweep removed.
type standDownSweptCounts struct {
	rhythms int
	waves   int
}

// standDownSweep deletes g's deploy-seeded group-target cron jobs and cancels
// any pending wave choreography, returning the counts. Each step is best-effort
// (logged, not fatal) — the leftover rows are self-healing, so a partial sweep
// never leaves the force in a broken state.
func standDownSweep(g *db.AgentGroup) standDownSweptCounts {
	var out standDownSweptCounts

	// Count the pending waves BEFORE deleting the choreography row, so the
	// report reflects what was actually cancelled.
	if choreo, err := db.GetWaveChoreography(g.ID); err != nil {
		slog.Warn("stand-down: could not read wave choreography", "group", g.Name, "err", err)
	} else if choreo != nil {
		out.waves = choreo.PendingWaves()
		if err := db.DeleteWaveChoreography(g.ID); err != nil {
			slog.Warn("stand-down: could not cancel wave choreography", "group", g.Name, "err", err)
			out.waves = 0
		}
	}

	if n, err := db.DeleteGroupTargetCronJobs(g.ID); err != nil {
		slog.Warn("stand-down: could not sweep rhythm cron jobs", "group", g.Name, "err", err)
	} else {
		out.rhythms = n
	}

	if out.rhythms > 0 || out.waves > 0 {
		slog.Info("stand-down swept deploy runtime",
			"group", g.Name, "rhythms_removed", out.rhythms, "waves_cancelled", out.waves)
	}
	return out
}

// standDownReason reads the optional ?reason= audit note for the retire leg.
func standDownReason(r *http.Request) string {
	return strings.TrimSpace(r.URL.Query().Get("reason"))
}
