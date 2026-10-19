package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
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
	// Unrestricted marks access through unrestricted trust.
	Unrestricted bool `json:"unrestricted,omitempty"`
	// UsedUp says a cap is spent: the gateway refuses every request.
	UsedUp bool `json:"used_up,omitempty"`
	// Scope is the shown grant's scope (http_proxy=<gateway>, or "" for
	// every gateway), which the panel's extend, raise and revoke act on.
	Scope string `json:"scope,omitempty"`
	// GrantExpiresAt is the shown grant's own end (ExpiresAt is access's,
	// across every grant); Pool names a node group the grant comes from,
	// which is administered as that group's grant, not the peer's.
	GrantExpiresAt *time.Time `json:"grant_expires_at,omitempty"`
	Pool           string     `json:"pool,omitempty"`
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
		s.Unrestricted = unrestricted
		if grant != nil {
			s.MaxUSD, s.Since, s.Scope = grant.SpawnPolicy.MaxUSD, grant.CreatedAt, grant.Scope
			s.GrantExpiresAt, s.Pool = grant.ExpiresAt, grant.PoolName
		}
		s.UsedUp = left <= 0
		// Access lasts as long as its longest-lived source: unrestricted
		// trust or a permanent grant never ends.
		if !unrestricted {
			for i := range grants {
				g := &grants[i]
				if g.Slug != PermModelsProxy || (g.Scope != "" && g.Scope != "http_proxy="+name) {
					continue
				}
				if g.ExpiresAt == nil {
					s.ExpiresAt = nil
					break
				}
				if s.ExpiresAt == nil || g.ExpiresAt.After(*s.ExpiresAt) {
					s.ExpiresAt = g.ExpiresAt
				}
			}
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
	if rt == nil {
		writeJSON(w, 200, map[string]any{"shares": []modelShare{}, "errors": map[string]string{}, "offline": true})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelSharesReceivedWait)
	defer cancel()
	out, errs := receivedModelShares(ctx, rt, peers)
	writeJSON(w, 200, map[string]any{"shares": out, "errors": errs})
}

// receivedModelShares asks each peer what it shares with this node; errs
// names the peers that could not answer.
func receivedModelShares(ctx context.Context, rt *fedRuntime, peers []db.FederationPeer) ([]modelShare, map[string]string) {
	out := []modelShare{}
	errs := map[string]string{}
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
			// A peer too old to know the route refuses it like any unknown
			// path; that is "shares nothing", anything else is an error.
			if reply.Status == http.StatusForbidden || reply.Status == http.StatusNotFound {
				return
			}
			if reply.Status != 200 || json.Unmarshal(reply.Body, &body) != nil {
				errs[peer.InstanceID] = fmt.Sprintf("unexpected answer (HTTP %d)", reply.Status)
				return
			}
			for _, s := range body.Shares {
				if !validReceivedShare(&s) {
					continue
				}
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
	return out, errs
}

// validReceivedShare checks what a peer says it shares before it reaches the
// dashboard or a terminal: names in their own charsets, a known dialect.
// A model entry outside the model-ID charset is dropped.
func validReceivedShare(s *modelShare) bool {
	if !validModelProxyName(s.Gateway) || (s.Dialect != "anthropic" && s.Dialect != "openai") || !(s.MaxUSD >= 0) || !(s.SpentUSD >= 0) {
		return false
	}
	if gw, ok := strings.CutPrefix(s.Scope, "http_proxy="); s.Scope != "" && (!ok || !validModelProxyName(gw)) {
		s.Scope = ""
	}
	models := s.Models[:0]
	for _, m := range s.Models {
		if validShareModelID(m) {
			models = append(models, m)
		}
	}
	s.Models = models
	return true
}

func validShareModelID(m string) bool {
	if m == "" || len(m) > 128 {
		return false
	}
	for _, r := range m {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alnum && !strings.ContainsRune("._-:/@[]*", r) {
			return false
		}
	}
	return true
}
