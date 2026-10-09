package agentd

import "net/http"

func registerFederationPeerViewRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/federation/peer/{node}/{tail...}", handleFederationPeerView)
	mux.HandleFunc("GET /v1/federation/node-summary", handleFederationLocalSummary)
}

func handleFederationPeerView(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "read a live peer view") {
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
