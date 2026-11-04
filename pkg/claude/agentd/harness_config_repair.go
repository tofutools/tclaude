package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/launchspec"
)

// One-off repair for agents whose explicit --harness-config posture was lost
// on a relaunch before tcl-bmhpir (#3817): resume, reincarnate and some
// clones rebuilt the policy from the profile chain without it, so a read pin
// over a write chain widened to write, and a write selection fell back to the
// floor (tcl-pa5589).
//
// The verbatim first spawn request (agents.initial_spawn_config) still names
// the posture asked for, so an agent whose recorded launch snapshot no longer
// carries it as the launch contract is flagged. Re-pinning writes it back the
// way a spawn records it (applySpawnHarnessConfig: the value, no provenance),
// which the next resume keeps (reapplyExplicitHarnessConfig). Nothing restarts
// here; a running agent takes it on its next resume or restart.
//
// Not found this way: an agent with no recorded spawn request (the
// pending-spawn sweeper, the group-template instantiator, promotion, legacy
// backfill), and a clone whose own spawn request did not name the posture.

// Changes a lost pin made, worst first.
const (
	harnessConfigRepairWidened  = "widened"  // asked for the floor, runs without it
	harnessConfigRepairNarrowed = "narrowed" // asked for write, runs under the floor
	harnessConfigRepairUnpinned = "unpinned" // same posture today, but a profile edit could change it
)

// harnessConfigRepairLimits is what the finder cannot see, shown with its results.
var harnessConfigRepairLimits = []string{
	"Only agents with a recorded spawn request are checked: agents created by the pending-spawn sweeper or a group template, promoted, or backfilled are not.",
	"A clone is checked against its own spawn request, so a clone that did not name the posture is not found.",
	"A re-pin changes the recorded launch; a running agent takes it on its next resume or restart.",
}

// harnessConfigRepair is one agent whose recorded launch lost its explicit
// harness_config.
type harnessConfigRepair struct {
	AgentID   string `json:"agent_id"`
	ConvID    string `json:"conv_id"`
	Title     string `json:"title,omitempty"`
	Requested string `json:"requested"`
	// Current is the posture the recorded launch runs with: read, write or
	// default (the floor). Source names the profile it came from, if any.
	Current string `json:"current"`
	Source  string `json:"source,omitempty"`
	Change  string `json:"change"`
	Online  bool   `json:"online"`
	// Retired is set only in an include_retired listing. A retired agent is
	// listed, never re-pinned.
	Retired bool `json:"retired,omitempty"`
}

// requestedHarnessConfig reads the explicit harness_config from a recorded
// spawn request, "" when it named none.
func requestedHarnessConfig(raw string) string {
	var req struct {
		HarnessConfig string `json:"harness_config"`
	}
	if json.Unmarshal([]byte(raw), &req) != nil {
		return ""
	}
	return strings.TrimSpace(req.HarnessConfig)
}

// harnessConfigRepairFor reports whether rec lost its explicit posture, and
// how. A snapshot that omits every profile tier never carries one, and a nil
// snapshot is the fail-closed legacy sentinel, so neither is flagged. Nor is a
// posture the relaunch policy (mode) gives up for the chain's: that is the
// policy working, not a lost pin.
func harnessConfigRepairFor(rec db.AgentLaunchRecord, mode launchspec.RelaunchMode) (harnessConfigRepair, bool) {
	requested := requestedHarnessConfig(rec.InitialSpawnConfig)
	snap := rec.EffectiveSandbox
	if requested == "" || snap == nil || snap.ProfilesOmitted {
		return harnessConfigRepair{}, false
	}
	if relaunchHarnessConfig(mode, snap.Effective.HarnessConfig, requested) == "" {
		return harnessConfigRepair{}, false
	}
	if explicitHarnessConfig(snap) == requested {
		return harnessConfigRepair{}, false
	}
	current := snap.Effective.HarnessConfig
	out := harnessConfigRepair{
		AgentID: rec.AgentID, ConvID: rec.ConvID,
		Requested: requested, Current: string(current), Change: harnessConfigRepairUnpinned,
		Retired: rec.Retired,
	}
	if current == sandboxpolicy.HarnessConfigAccessDefault {
		out.Current = "default"
	}
	if p := snap.Effective.Provenance.HarnessConfig; p != nil {
		out.Source = p.Profile
	}
	wantFloor := sandboxpolicy.HarnessConfigFloorApplies(sandboxpolicy.HarnessConfigAccess(requested))
	hasFloor := sandboxpolicy.HarnessConfigFloorApplies(current)
	switch {
	case wantFloor && !hasFloor:
		out.Change = harnessConfigRepairWidened
	case !wantFloor && hasFloor:
		out.Change = harnessConfigRepairNarrowed
	}
	return out, true
}

// findHarnessConfigRepairs lists every active agent that lost its explicit
// posture, widened ones first. Retired agents are skipped in the query unless
// includeRetired (the CLI's read-only --include-retired).
func findHarnessConfigRepairs(includeRetired bool) ([]harnessConfigRepair, error) {
	records, err := db.ListAgentLaunchRecords(includeRetired)
	if err != nil {
		return nil, err
	}
	out := []harnessConfigRepair{}
	mode := currentRelaunchPolicy().Mode(harnessConfigField)
	for _, rec := range records {
		repair, ok := harnessConfigRepairFor(rec, mode)
		if !ok {
			continue
		}
		if row, _ := db.GetConvIndex(rec.ConvID); row != nil {
			repair.Title = agent.DisplayTitle(row)
		}
		if !rec.Retired {
			repair.Online = isConvOnline(rec.ConvID)
		}
		out = append(out, repair)
	}
	rank := map[string]int{harnessConfigRepairWidened: 0, harnessConfigRepairNarrowed: 1, harnessConfigRepairUnpinned: 2}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].Change] < rank[out[j].Change] })
	return out, nil
}

// repinHarnessConfig writes agentID's requested posture back into its
// recorded launch, judged afresh against what is stored now: an agent no
// longer in need of repair is left alone and reported so.
//
// It holds the actor's launch lock, which resume, restart and reincarnate hold
// across their read of the stored snapshot and every write of it (including a
// failed launch's restore). Without it a relaunch already in flight would
// write back the unpinned snapshot it read before the re-pin, after the
// re-pin reported success. The compare-and-swap below guards the remaining
// writers that do not take the lock.
func repinHarnessConfig(agentID string) (harnessConfigRepair, error) {
	conv, err := db.CurrentConvForAgent(agentID)
	if err != nil {
		return harnessConfigRepair{}, err
	}
	if conv == "" {
		return harnessConfigRepair{}, errHarnessConfigNotNeeded
	}
	launchLock := resumeLaunchLock(conv)
	launchLock.Lock()
	defer launchLock.Unlock()
	rec, err := db.ActiveAgentLaunchRecord(agentID)
	if err != nil {
		return harnessConfigRepair{}, err
	}
	if rec == nil {
		return harnessConfigRepair{}, errHarnessConfigNotNeeded
	}
	repair, ok := harnessConfigRepairFor(*rec, currentRelaunchPolicy().Mode(harnessConfigField))
	if !ok {
		return harnessConfigRepair{}, errHarnessConfigNotNeeded
	}
	applied, fail := applySpawnHarnessConfig(*rec.EffectiveSandbox, repair.Requested)
	if fail != nil {
		return repair, errors.New(fail.Msg)
	}
	if err := db.ReplaceAgentEffectiveSandboxConfig(rec.AgentID, rec.EffectiveSandboxRaw, &applied); err != nil {
		return repair, err
	}
	return repair, nil
}

var errHarnessConfigNotNeeded = errors.New("no lost harness_config pin to restore (not an active agent, or already pinned)")

// harnessConfigRepairResult is one agent's outcome in a re-pin batch.
type harnessConfigRepairResult struct {
	AgentID string `json:"agent_id"`
	Repined bool   `json:"repinned"`
	Change  string `json:"change,omitempty"`
	Online  bool   `json:"online,omitempty"`
	Error   string `json:"error,omitempty"`
}

// serveHarnessConfigRepairs is GET (the read-only finder) and POST {agent_ids}
// (re-pin those agents) of the harness_config repair.
func serveHarnessConfigRepairs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		repairs, err := findHarnessConfigRepairs(includeRetiredParam(r))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"agents": repairs, "limits": harnessConfigRepairLimits})
	case http.MethodPost:
		var body struct {
			AgentIDs []string `json:"agent_ids"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil || len(body.AgentIDs) == 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg", `body must be {"agent_ids": ["..."]}`)
			return
		}
		results := make([]harnessConfigRepairResult, 0, len(body.AgentIDs))
		var repinned []string
		for _, id := range body.AgentIDs {
			id = strings.TrimSpace(id)
			res := harnessConfigRepairResult{AgentID: id}
			repair, err := repinHarnessConfig(id)
			if err != nil {
				res.Error = err.Error()
			} else {
				res.Repined, res.Change = true, repair.Change
				res.Online = isConvOnline(repair.ConvID)
				repinned = append(repinned, fmt.Sprintf("%s=%s", id, repair.Requested))
			}
			results = append(results, res)
		}
		if len(repinned) > 0 {
			setAuditTargetLabel(r, "harness_config re-pinned: "+strings.Join(repinned, ", "))
			invalidateDoctorChecks()
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET or POST")
	}
}

// handleHarnessConfigRepairs is /v1/harness-config-repairs on the agent
// socket, and /api/harness-config-repairs on the dashboard (as the operator):
// sandbox posture is sandbox-profiles.manage's to change, and that gate keeps
// the finder as well, since it reads other agents' launch records.
func handleHarnessConfigRepairs(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermSandboxProfilesManage); !ok {
		return
	}
	serveHarnessConfigRepairs(w, r)
}
