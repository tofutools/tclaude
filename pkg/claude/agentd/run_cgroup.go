package agentd

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// prepareRunCgroup is the host cgroup seam; flow tests swap it because the
// test process has no delegated cgroup subtree.
var prepareRunCgroup = session.PrepareRunCgroup

// handleRunCgroup moves the calling `tclaude run` process into a fresh cgroup
// and holds it for the life of the connection. The peer PID comes from
// SO_PEERCRED, so a caller can only ever move itself. The response line is
// written once the move is done; the caller then starts its child, which
// inherits the cgroup. When the connection closes — normally because the
// caller exited — the cgroup's remaining members are killed and it is removed.
//
// No permission slug gates this: the new cgroup's limits are clamped to the
// caller's existing ceilings, so it can only narrow what the caller may use.
func handleRunCgroup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	if _, _, ok := authedCaller(w, r); !ok {
		return
	}
	p := peerFromContext(r.Context())
	if p.DashboardHuman || p.PID <= 1 {
		writeError(w, http.StatusBadRequest, "peer", "a run cgroup needs a local socket caller")
		return
	}
	var req agent.RunCgroupRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "decode body: "+err.Error())
		return
	}
	// Consume the rest of the body so the server watches the connection and
	// cancels the request context when the caller goes away.
	_, _ = io.Copy(io.Discard, r.Body)
	dir, applied, release, err := prepareRunCgroup(p.PID, req.Limits)
	if err != nil {
		writeError(w, http.StatusConflict, "cgroup_unavailable", err.Error())
		return
	}
	defer release()
	writeJSON(w, http.StatusOK, agent.RunCgroupResponse{Cgroup: dir, Limits: applied})
	// The middleware wrappers expose Unwrap, so the controller reaches the
	// connection's own flusher.
	if err := http.NewResponseController(w).Flush(); err != nil {
		slog.Warn("run cgroup: flush response", "error", err)
	}
	slog.Debug("run cgroup: holding", "pid", p.PID, "cgroup", dir)
	<-r.Context().Done()
}
