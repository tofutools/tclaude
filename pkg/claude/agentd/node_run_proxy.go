package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/noderun"
)

// Audits both sides of a remote script operation without persisting its body
// on the sender. Only the receiving node stores the script and logs.
func auditNodeRunProxy(r *http.Request, peer string, body []byte, status int, reply []byte) {
	tail := r.PathValue("tail")
	if r.Method == http.MethodPost && tail == "node/run" {
		var req noderun.Request
		if json.Unmarshal(body, &req) != nil || req.Validate() != nil {
			return
		}
		hash := sha256.Sum256([]byte(req.Script))
		j := noderun.Job{Actor: "operator", Peer: peer, Node: peer, ScriptSHA256: hex.EncodeToString(hash[:]), ScriptBytes: len(req.Script), State: "failed", ExitCode: 125}
		if status == 202 {
			var remote noderun.Job
			if json.Unmarshal(reply, &remote) == nil && noderun.ValidID(remote.ID) {
				j.ID = remote.ID
				j.State = "running"
				j.ExitCode = -1
			}
		}
		recordNodeRunAudit(j, "sent")
	}
	if r.Method == http.MethodGet && strings.HasPrefix(tail, "node/run/jobs/") && !strings.HasSuffix(tail, "/logs") && status == 200 {
		var j noderun.Job
		if json.Unmarshal(reply, &j) != nil || !noderun.ValidID(j.ID) || j.State == "running" {
			return
		}
		// Peer-controlled fields never select audit identity or inject a detail.
		if len(j.ScriptSHA256) != 64 || strings.Trim(j.ScriptSHA256, "0123456789abcdef") != "" {
			return
		}
		switch j.State {
		case "completed", "failed", "canceled", "interrupted":
		default:
			return
		}
		j.Actor = "operator"
		j.Peer = peer
		j.Node = peer
		recordNodeRunAudit(j, "observed_result")
	}
}

func auditNodeRunProxyRequest(r *http.Request, peer string, body []byte) {
	if r.Method != http.MethodPost || r.PathValue("tail") != "node/run" {
		return
	}
	var req noderun.Request
	if json.Unmarshal(body, &req) != nil || req.Validate() != nil {
		return
	}
	hash := sha256.Sum256([]byte(req.Script))
	recordNodeRunAudit(noderun.Job{Actor: "operator", Peer: peer, Node: peer, ScriptSHA256: hex.EncodeToString(hash[:]), ScriptBytes: len(req.Script), State: "requested", ExitCode: -1}, "requested")
}
