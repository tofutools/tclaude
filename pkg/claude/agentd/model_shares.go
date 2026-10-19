package agentd

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// modelShare is one gateway a peer may send model calls through (usage
// sharing): what it serves, its dollar cap and spend, and when it ends.
// Peer is the node on the other end: the receiving peer in what this node
// shares, the gateway's node in what is shared with it.
type modelShare struct {
	Peer      string     `json:"peer"`
	PeerName  string     `json:"peer_name,omitempty"`
	Gateway   string     `json:"gateway"`
	Dialect   string     `json:"dialect"`
	Models    []string   `json:"models"`
	MaxUSD    float64    `json:"max_usd,omitempty"`
	SpentUSD  float64    `json:"spent_usd"`
	Since     time.Time  `json:"since,omitzero"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// Unrestricted marks access through unrestricted trust, not a grant.
	Unrestricted bool `json:"unrestricted,omitempty"`
}

// modelSharesFor lists the gateways peer may use on this node now. A
// gateway the peer is blocked on, or that is off or misconfigured, is left
// out: it serves nothing.
func modelSharesFor(cfg *config.Config, peer *db.FederationPeer) []modelShare {
	out := []modelShare{}
	if cfg == nil || cfg.Agent == nil || cfg.Agent.ModelProxyDisabled {
		return out
	}
	names := make([]string, 0, len(cfg.Agent.HTTPProxies))
	for name, gw := range cfg.Agent.HTTPProxies {
		if gw.ModelPolicy != nil && gw.ModelPolicy.Enabled {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	grants, _ := db.ListEffectiveFederationPeerGrants(peer.InstanceID)
	unrestricted := db.FederationPeerUnrestricted(peer.InstanceID)
	for _, name := range names {
		if !fedPeerModelAllows(peer.InstanceID, name) {
			continue
		}
		p := cfg.Agent.HTTPProxies[name].ModelPolicy
		s := modelShare{Peer: peer.InstanceID, PeerName: peerDisplay(peer), Gateway: name, Dialect: p.Dialect, Models: slices.Clone(p.Models)}
		if s.Dialect == "" {
			s.Dialect = "anthropic"
		}
		// The grant shown is the tightest capped one (least left), else the
		// one most specific to this gateway.
		var grant *db.FederationPeerGrant
		left := int64(math.MaxInt64)
		for _, g := range modelShareGrants(peer.InstanceID, name) {
			c := shareCap(g)
			spent, err := db.ModelProxyShareSpent(peer.InstanceID, shareProxy(c, name), c.Since)
			if err == nil && c.MicroUSD-spent < left {
				left = c.MicroUSD - spent
				grant = &g
				s.SpentUSD = float64(spent) / 1e6
			}
		}
		if grant == nil {
			for i := range grants {
				g := &grants[i]
				if g.Slug == PermModelsProxy && (g.Scope == "" || g.Scope == "http_proxy="+name) && (grant == nil || g.Scope != "") {
					grant = g
				}
			}
			since := time.Time{}
			if grant != nil {
				since = grant.CreatedAt
			}
			proxy := name
			if grant != nil && grant.Scope == "" {
				proxy = ""
			}
			if spent, err := db.ModelProxyShareSpent(peer.InstanceID, proxy, since); err == nil {
				s.SpentUSD = float64(spent) / 1e6
			}
		}
		if grant == nil {
			s.Unrestricted = unrestricted
		} else {
			s.MaxUSD, s.Since, s.ExpiresAt = grant.SpawnPolicy.MaxUSD, grant.CreatedAt, grant.ExpiresAt
		}
		out = append(out, s)
	}
	return out
}

// handleModelSharesGiven lists what this node shares with every trusted peer.
func handleModelSharesGiven(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "inspect usage sharing") {
		return
	}
	cfg, _ := config.Load()
	peers, err := db.ListFederationPeers()
	if err != nil {
		writeFedErr(w, err)
		return
	}
	out := []modelShare{}
	for i := range peers {
		out = append(out, modelSharesFor(cfg, &peers[i])...)
	}
	writeJSON(w, 200, map[string]any{"shares": out})
}

// servePeerModelShares answers a peer with the gateways it may use here.
// Any trusted peer may ask; one with no access gets an empty list.
func servePeerModelShares(w http.ResponseWriter, _ *http.Request, v *peerView, _ peerViewRule) {
	cfg, _ := config.Load()
	writeJSON(w, 200, map[string]any{"shares": modelSharesFor(cfg, v.peer)})
}

// modelSharesReceivedWait bounds the fan-out to every peer.
const modelSharesReceivedWait = 8 * time.Second

// handleModelSharesReceived asks every trusted peer what it shares with this
// node. An unreachable peer is listed under errors; a peer too old to answer
// shares nothing.
func handleModelSharesReceived(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "inspect usage sharing") {
		return
	}
	peers, err := db.ListFederationPeers()
	if err != nil {
		writeFedErr(w, err)
		return
	}
	rt := currentFederation()
	out := []modelShare{}
	errs := map[string]string{}
	if rt == nil {
		writeJSON(w, 200, map[string]any{"shares": out, "errors": errs, "offline": true})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelSharesReceivedWait)
	defer cancel()
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range peers {
		peer := &peers[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			reply, err := peerReads.read(ctx, rt, peer, fedPeerViewRequest{Method: http.MethodGet, URI: "/api/model-shares", Header: http.Header{}})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[peer.InstanceID] = err.Error()
				return
			}
			var body struct {
				Shares []modelShare `json:"shares"`
			}
			if reply.Status != 200 || json.Unmarshal(reply.Body, &body) != nil {
				return
			}
			for _, s := range body.Shares {
				// The answer names this node; restate it as the gateway's node.
				s.Peer, s.PeerName = peer.InstanceID, peerDisplay(peer)
				out = append(out, s)
			}
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool {
		if out[i].PeerName != out[j].PeerName {
			return out[i].PeerName < out[j].PeerName
		}
		return out[i].Gateway < out[j].Gateway
	})
	writeJSON(w, 200, map[string]any{"shares": out, "errors": errs})
}
