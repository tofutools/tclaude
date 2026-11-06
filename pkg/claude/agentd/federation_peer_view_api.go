package agentd

import (
	"encoding/json"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func registerFederationPeerViewRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/federation/peer/{node}/{tail...}", handleFederationPeerView)
	mux.HandleFunc("POST /v1/federation/peer/{node}/{tail...}", handleFederationPeerView)
	mux.HandleFunc("PATCH /v1/federation/peer/{node}/{tail...}", handleFederationPeerView)
	mux.HandleFunc("PUT /v1/federation/peer/{node}/{tail...}", handleFederationPeerView)
	mux.HandleFunc("DELETE /v1/federation/peer/{node}/{tail...}", handleFederationPeerView)
	mux.HandleFunc("GET /v1/federation/node-summary", handleFederationLocalSummary)
}

func handleFederationPeerView(w http.ResponseWriter, r *http.Request) {
	caller, human, ok := authedCaller(w, r)
	if !ok {
		return
	}
	if !human {
		// An agent's view acts under its peer= grants (peer_view_agents.go).
		servePeerViewForAgent(w, r, caller, r.PathValue("node"))
		return
	}
	// Only operator-chosen labels and pinned identities select a destination.
	peer, err := resolveFederationPeerOpt(r.PathValue("node"), false)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	r.SetPathValue("node", peer.InstanceID)
	servePeerViewProxy(w, r)
}

func handleFederationLocalSummary(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "read the local node summary") {
		return
	}
	serveLocalNodeSummary(w, r)
}

// handleFederationPeerAgentViews reads (GET ?peer=) or sets (POST {instance,
// policy}) whether this node serves peer views a trusted peer's agents open.
// The default is allow: the peer's grants still bound every request, and
// the peer's agents also need their own operator's peer= grants.
func handleFederationPeerAgentViews(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "manage peer agent views") {
		return
	}
	ref := r.URL.Query().Get("peer")
	var in struct {
		Instance string `json:"instance"`
		Policy   string `json:"policy"`
	}
	if r.Method == http.MethodPost {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "expected {instance, policy}")
			return
		}
		ref = in.Instance
		if in.Policy != db.FederationPeerAgentViewsAllow && in.Policy != db.FederationPeerAgentViewsDeny {
			writeError(w, http.StatusBadRequest, "invalid_arg", "policy must be allow or deny")
			return
		}
	}
	peer, err := resolveFederationPeerOpt(ref, false)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	if r.Method == http.MethodPost {
		if err := db.SetFederationPeerAgentViews(peer.InstanceID, in.Policy); err != nil {
			writeError(w, http.StatusServiceUnavailable, "io", "could not save the agent view policy")
			return
		}
	}
	policy, err := db.GetFederationPeerAgentViews(peer.InstanceID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "io", "could not read the agent view policy")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"peer": peer.InstanceID, "label": peer.Label, "policy": policy})
}
