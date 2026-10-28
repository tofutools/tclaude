package agentd

import (
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
	doctorCached = doctor.Run(doctorProbes())
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
