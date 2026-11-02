package agentd

import (
	"net/http"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The dashboard's Spawn queue block (tcl-ao1yz5): the capacity line and the
// spawns waiting for capacity, local and remote, in queue order. Not to be
// confused with queued_spawns, the spawn-dialog drafts (queued_spawns.go).

// collectCapacityQueueSnapshot is the operator's view of `tclaude agent queue
// ls`. Nil when no capacity limit is set and nothing waits, so the block stays
// hidden for everyone who does not use capacity limits.
func collectCapacityQueueSnapshot() *spawnQueueListJSON {
	limits, err := nodeCapacityLimits()
	if err != nil {
		return nil
	}
	entries, err := db.ListSpawnQueue(false)
	if err != nil || (!limits.Limited() && len(entries) == 0) {
		return nil
	}
	out := &spawnQueueListJSON{Capacity: spawnQueueCapacity(limits), Entries: make([]spawnQueueEntryJSON, 0, len(entries))}
	now := time.Now()
	for _, e := range entries {
		out.Capacity.Queued++
		out.Entries = append(out.Entries, spawnQueueView(e, now))
	}
	return out
}

func registerDashboardSpawnQueueRoutes(mux *http.ServeMux) {
	// The operator cancels as from `tclaude agent queue rm`.
	mux.HandleFunc("DELETE /api/spawn-queue/{id}", dashboardRolesRoute(handleSpawnQueueCancel))
}
