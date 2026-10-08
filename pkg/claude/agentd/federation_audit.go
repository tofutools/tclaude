package agentd

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

const PermFederationAuditRead = "federation.audit.read"

func handleFederationAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermFederationAuditRead, ActionContext{}); !ok {
		return
	}
	peer := strings.TrimSpace(r.URL.Query().Get("peer"))
	if peer != "" && !proto.ValidInstanceID(peer) {
		p, err := resolveFederationPeer(peer)
		if err != nil {
			writeFedErr(w, err)
			return
		}
		peer = p.InstanceID
	}
	since := time.Time{}
	if value := r.URL.Query().Get("since"); value != "" {
		var err error
		since, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			writeError(w, 400, "invalid_arg", "since must be an RFC3339 timestamp")
			return
		}
	}
	limit := 200
	if value := r.URL.Query().Get("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, 400, "invalid_arg", "limit must be 1..1000")
			return
		}
		limit = n
	}
	rows, err := db.ListFederationActivity(peer, since, limit)
	if err != nil {
		writeError(w, 500, "audit_unavailable", fmt.Sprintf("federation audit unavailable: %v", err))
		return
	}
	writeJSON(w, 200, rows)
}
