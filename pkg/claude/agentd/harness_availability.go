package agentd

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/nodeinfo"
)

const PermNodeHarnessesRead = "node.harnesses.read"

var availabilityCache struct {
	sync.Mutex
	value nodeinfo.Availability
}

func cachedHarnessAvailability(refresh bool) nodeinfo.Availability {
	availabilityCache.Lock()
	defer availabilityCache.Unlock()
	now := time.Now()
	// Single-flight probing and a short refresh cooldown bound repeated manual
	// requests. Probes have their own total deadline, independent of callers.
	if availabilityCache.value.Schema == 0 || now.After(availabilityCache.value.RefreshAfter) || refresh && now.Sub(availabilityCache.value.ObservedAt) >= 10*time.Second {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		availabilityCache.value = nodeinfo.ProbeAvailability(ctx)
	}
	return availabilityCache.value
}
func registerHarnessAvailabilityRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/harnesses/availability", handleDashboardHarnessAvailability)
}
func handleDashboardHarnessAvailability(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	serveHarnessAvailability(w, r)
}
func handleLocalHarnessAvailability(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "read harness availability") {
		return
	}
	serveHarnessAvailability(w, r)
}
func serveHarnessAvailability(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, cachedHarnessAvailability(r.URL.Query().Get("refresh") == "1"))
}
func servePeerHarnessAvailability(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if !v.allows(rule, 0) {
		writeError(w, 403, "permission_denied", "harness availability is not shared")
		return
	}
	out := cachedHarnessAvailability(r.URL.Query().Get("refresh") == "1")
	if !v.allows(rule, 0) {
		writeError(w, 403, "permission_denied", "harness availability is no longer shared")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, 200, out)
}
