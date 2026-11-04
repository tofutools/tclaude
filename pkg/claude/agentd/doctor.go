package agentd

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/doctor"
)

// The Diagnostics › Doctor view polls for its nav badge, and one of its checks is the live
// tclaude-layer probe, which forks bwrap on Linux. Results are therefore
// cached for doctorCacheTTL; the tab's Re-check button and any write that
// changes an answer (the default-sandbox decision) drop the cache.
const doctorCacheTTL = time.Minute

var doctorProbes = doctor.HostProbes

var (
	doctorMu       sync.Mutex
	doctorCached   []doctor.Check
	doctorCachedAt time.Time
)

func invalidateDoctorChecks() {
	doctorMu.Lock()
	doctorCached = nil
	doctorMu.Unlock()
}

func doctorChecks(refresh bool) []doctor.Check {
	doctorMu.Lock()
	defer doctorMu.Unlock()
	if !refresh && doctorCached != nil && time.Since(doctorCachedAt) < doctorCacheTTL {
		return doctorCached
	}
	doctorCached = append(doctor.Run(doctorProbes()), harnessConfigRepairChecks()...)
	doctorCachedAt = time.Now()
	return doctorCached
}

// handleDashboardDoctor answers GET /api/doctor[?refresh=1]. Read-only: every
// fix is either a command for the operator to run or a separate, permission-
// gated dashboard write.
func handleDashboardDoctor(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"checks": doctorChecks(r.URL.Query().Get("refresh") == "1"),
	})
}

// harnessConfigRepairChecks flags agents whose explicit harness_config was
// lost on a relaunch (harness_config_repair.go). It is daemon state, not a
// host probe, so it is checked here rather than in doctor.Run.
func harnessConfigRepairChecks() []doctor.Check {
	repairs, err := findHarnessConfigRepairs()
	if err != nil || len(repairs) == 0 {
		return nil
	}
	widened := 0
	for _, r := range repairs {
		if r.Change == harnessConfigRepairWidened {
			widened++
		}
	}
	severity := doctor.SeverityInfo
	if widened > 0 {
		severity = doctor.SeverityWarning
	}
	return []doctor.Check{{
		ID: "harness-config-repair", Category: doctor.CategorySandbox, Severity: severity,
		Title: fmt.Sprintf("%d agent(s) lost the harness_config posture they were spawned with", len(repairs)),
		Detail: fmt.Sprintf("A relaunch before the fix in #3817 dropped an explicit --harness-config; %d of them now run "+
			"without the read-only floor they asked for. Review them and restore the posture. Agents with no recorded spawn "+
			"request (sweeper or group-template spawns) cannot be checked.", widened),
		Fix:    "tclaude agent sandbox-profiles harness-config-repairs",
		Action: doctor.ActionHarnessConfigRepair,
	}}
}
