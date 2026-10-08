package agentd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/tofutools/tclaude/pkg/federation/jobrepo"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Fan-out pins content before submitting anything. It is an explicit list of
// independent jobs, not an atomic distributed transaction or a placement queue.
func handleFederationJobFanout(w http.ResponseWriter, r *http.Request, q proto.JobRequest, nodes []string) {
	if len(nodes) > 32 {
		writeError(w, 400, "nodes", "at most 32 explicit nodes are allowed")
		return
	}
	if len(nodes) > 1 && !jobrepo.FullCommit(q.Ref) {
		writeError(w, 400, "ref", "multi-node jobs require --ref with a full commit SHA; resolve a branch with e.g. git rev-parse origin/<branch>")
		return
	}
	q.ID = proto.NewEnvelopeID()
	if e := validateJob(&q); e != nil {
		writeError(w, 400, "job", e.Error())
		return
	}
	peers := []string{}
	seen := map[string]bool{}
	for _, node := range nodes {
		if node == "auto" || strings.HasPrefix(node, "group:") {
			writeError(w, 400, "nodes", "fan-out requires explicit --node values; auto or group:<pool> selects one node")
			return
		}
		p, e := resolveFederationPeerOpt(node, false)
		if e != nil {
			writeFedErr(w, e)
			return
		}
		if seen[p.InstanceID] {
			writeError(w, 400, "nodes", "duplicate node in fan-out")
			return
		}
		seen[p.InstanceID] = true
		if !jobCallerAllowed(w, r, p.InstanceID, q.Group) {
			return
		}
		cat, _, e := fedCatalogFor(p.InstanceID)
		found := false
		if e == nil && cat != nil {
			for _, g := range cat.Groups {
				if g.Name == q.Group && g.HasCap(proto.CapJobs) {
					found = true
				}
			}
		}
		if !found {
			writeError(w, 403, "not_exported", "node "+node+" does not export this group for jobs")
			return
		}
		peers = append(peers, p.InstanceID)
	}
	results := []map[string]any{}
	for _, peer := range peers {
		raw, _ := json.Marshal(struct {
			proto.JobRequest
			Node string `json:"node"`
		}{q, peer})
		sub := r.Clone(r.Context())
		sub.Body = io.NopCloser(bytes.NewReader(raw))
		rec := httptest.NewRecorder()
		handleFederationJobSend(rec, sub)
		var result map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &result)
		if result == nil {
			result = map[string]any{}
		}
		result["peer"] = peer
		result["status"] = rec.Code
		results = append(results, result)
	}
	writeJSON(w, 200, map[string]any{"results": results})
}
