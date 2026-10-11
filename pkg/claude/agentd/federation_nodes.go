package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/nodeinfo"
)

const PermNodeRead = "node.read"
const fedNodeRefresh = 30 * time.Second
const fedNodeStaleAfter = 90 * time.Second

func fedPeerReadsNode(peer string) bool {
	if p, _ := db.GetFederationPeer(peer); p == nil {
		return false
	}
	if db.FederationPeerUnrestricted(peer) {
		return true
	}
	grants, err := db.ListEffectiveFederationPeerGrants(peer)
	if err != nil {
		return false
	}
	for _, g := range grants {
		if g.Slug == PermNodeRead && g.Scope == "" {
			return true
		}
	}
	return false
}
func (rt *fedRuntime) wakeNodes() {
	select {
	case rt.nodeWake <- struct{}{}:
	default:
	}
}

// A separate worker keeps version subprocesses and node fanout off the inbound
// worker. No probing or updates without a connected, authorized online peer.
func (rt *fedRuntime) nodeLoop(ctx context.Context) {
	ticker := time.NewTicker(fedNodeRefresh)
	defer ticker.Stop()
	var probed time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-rt.nodeWake:
		}
		peers, _ := db.ListFederationPeers()
		recipients := []string{}
		for _, p := range peers {
			if rt.sessionPeerOnline(p.InstanceID) && fedPeerReadsNode(p.InstanceID) {
				recipients = append(recipients, p.InstanceID)
			}
		}
		if len(recipients) == 0 {
			continue
		}
		if time.Since(probed) > 5*time.Minute {
			n := nodeinfo.Probe(ctx)
			rt.nodeMu.Lock()
			rt.nodeStatic = n
			rt.nodeMu.Unlock()
			probed = time.Now()
		}
		for _, peer := range recipients {
			// Authority can change while probing. Never publish from a stale recipient list.
			if !rt.sessionPeerOnline(peer) || !fedPeerReadsNode(peer) {
				continue
			}
			rt.sendControl(peer, proto.KindNodeUpdate, "", proto.NodeUpdatePayload{Node: localNodeMetadata(), At: time.Now().UTC()})
		}
	}
}
func localNodeMetadata() *proto.NodeMetadata {
	n := nodeinfo.Base()
	if rt := currentFederation(); rt != nil {
		rt.nodeMu.RLock()
		if rt.nodeStatic.Schema == 1 {
			n = rt.nodeStatic
		}
		rt.nodeMu.RUnlock()
	}
	cfg, err := config.Load()
	// Unknown configuration must not advertise zero as unlimited capacity.
	if err != nil || cfg == nil {
		return nil
	}
	if cfg.Federation != nil {
		n.Labels = append([]string{}, cfg.Federation.NodeLabels...)
		n.MaxLiveAgents = cfg.Federation.MaxLiveAgents
	}
	h := cachedHostStatus()
	r := proto.NodeResources{Status: h.Status, CPU: proto.NodeCPU{LogicalCores: h.CPU.LogicalCores, LoadAverage: h.CPU.LoadAverage}}
	if !h.ObservedAt.IsZero() {
		at := h.ObservedAt
		r.ObservedAt = &at
	}
	if h.RAM != nil {
		r.RAM = &proto.NodeRAM{TotalBytes: h.RAM.TotalBytes, AvailableBytes: h.RAM.AvailableBytes, AvailableEstimated: h.RAM.AvailableEstimated}
	}
	if h.Tclaude != nil {
		r.Agents = &proto.NodeAgents{LiveAgents: h.Tclaude.LiveAgents, LiveSessions: h.Tclaude.LiveSessions}
	}
	workOK, workCount := true, 0
	for _, d := range h.Disks {
		if d.Kind == "data" && d.Error == "" {
			r.DataDisk = &proto.NodeDisk{TotalBytes: d.TotalBytes, AvailableBytes: d.AvailableBytes}
		}
		if d.Kind != "work" {
			continue
		}
		workCount++
		if d.Error != "" || d.TotalBytes == 0 {
			workOK = false
			continue
		}
		bytes, percent := d.AvailableBytes, 100*float64(d.AvailableBytes)/float64(d.TotalBytes)
		if r.WorkDiskMinAvailableBytes == nil || bytes < *r.WorkDiskMinAvailableBytes {
			r.WorkDiskMinAvailableBytes = &bytes
		}
		if r.WorkDiskMinAvailablePercent == nil || percent < *r.WorkDiskMinAvailablePercent {
			r.WorkDiskMinAvailablePercent = &percent
		}
	}
	if !workOK || workCount == 0 {
		r.WorkDiskMinAvailableBytes = nil
		r.WorkDiskMinAvailablePercent = nil
	}
	// A missing group/config read means the required work-root set is unknown.
	for _, e := range h.Errors {
		if strings.HasPrefix(e, "Work directories:") || strings.HasPrefix(e, "Config:") {
			r.WorkDiskMinAvailableBytes = nil
			r.WorkDiskMinAvailablePercent = nil
		}
	}
	n.Resources = r
	return proto.SanitizeNode(&n)
}

// Full catalogs and updates share ordering, including a catalog's withdrawal.
// Local receipt times are overwritten; peers cannot refresh old observations by
// supplying a fabricated cache timestamp.
func mergeNodePublication(cat, previous *proto.CatalogPayload, at, created time.Time) {
	now := time.Now().UTC()
	if at.IsZero() {
		at = created
	}
	if at.After(created.Add(time.Second)) || created.After(now.Add(2*time.Minute)) {
		if previous != nil {
			cat.Node, cat.NodeAt, cat.NodeReceivedAt = previous.Node, previous.NodeAt, previous.NodeReceivedAt
		} else {
			cat.Node = nil
			cat.NodeAt = time.Time{}
			cat.NodeReceivedAt = time.Time{}
		}
		return
	}
	if previous != nil && !at.After(previous.NodeAt) {
		cat.Node, cat.NodeAt, cat.NodeReceivedAt = previous.Node, previous.NodeAt, previous.NodeReceivedAt
		return
	}
	cat.Node = proto.SanitizeNode(cat.Node)
	cat.NodeAt = at
	cat.NodeReceivedAt = now
	// Already delayed in transit is stale even immediately after receipt.
	if cat.Node != nil && now.Sub(created) > fedNodeStaleAfter {
		cat.Node.Resources.Status = "stale"
	}
}
func (rt *fedRuntime) acceptNodeUpdate(from string, e *proto.Envelope) {
	var u proto.NodeUpdatePayload
	if e.DecodePayload(&u) != nil {
		return
	}
	previous, catalogAt, err := fedCatalogFor(from)
	if err != nil {
		return
	}
	if previous == nil {
		previous = &proto.CatalogPayload{Groups: []proto.CatalogGroup{}}
	}
	next := *previous
	next.Node = u.Node
	mergeNodePublication(&next, previous, u.At, e.CreatedAt)
	clean, err := json.Marshal(next)
	if err == nil {
		if db.PutFederationCatalog(from, string(clean), catalogAt) == nil {
			rt.observeFleetNode(from, &next, time.Now())
		}
	}
}

type fedRemoteNode struct {
	*proto.NodeMetadata
	Peer       string    `json:"peer"`
	Instance   string    `json:"instance"`
	Online     bool      `json:"online"`
	ReceivedAt time.Time `json:"received_at"`
	Stale      bool      `json:"stale"`
}

func handleFederationNodes(w http.ResponseWriter, r *http.Request) {
	caller, human, ok := authedCaller(w, r)
	if !ok {
		return
	}
	match, err := proto.ParseNodeMatch(r.URL.Query().Get("match"))
	if err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return
	}
	peers, err := db.ListFederationPeers()
	if err != nil {
		writeFedErr(w, err)
		return
	}
	out := []fedRemoteNode{}
	rt := currentFederation()
	for _, p := range peers {
		if !human {
			allowed, _, err := permissionAllowsAction(r, caller, PermNodeRead, ActionContext{RemotePeer: p.InstanceID})
			if err != nil || !allowed {
				continue
			}
		}
		cat, _, err := fedCatalogFor(p.InstanceID)
		if err != nil || cat == nil || !match.Matches(cat.Node) {
			continue
		}
		online := rt != nil && rt.sessionPeerOnline(p.InstanceID)
		n := *cat.Node
		n.Resources = cat.Node.Resources
		stale := !online || cat.NodeReceivedAt.IsZero() || time.Since(cat.NodeReceivedAt) > fedNodeStaleAfter || n.Resources.Status != "current"
		if at := n.Resources.ObservedAt; at == nil || time.Since(*at) > fedNodeStaleAfter || at.After(time.Now().Add(2*time.Minute)) {
			stale = true
		}
		if stale && n.Resources.Status == "current" {
			n.Resources.Status = "stale"
		}
		out = append(out, fedRemoteNode{NodeMetadata: &n, Peer: peerDisplay(&p), Instance: p.InstanceID, Online: online, ReceivedAt: cat.NodeReceivedAt, Stale: stale})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Peer < out[j].Peer })
	writeJSON(w, 200, out)
}

// Incremental set operations preserve other writers' changes. Profiles can use
// the same local set without an ownership field or CLI-exclusive storage.
func handleFederationNodeLabels(w http.ResponseWriter, r *http.Request) {
	if !requireHuman(w, r, "manage local node labels") {
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		cfg, err := config.Load()
		if err != nil {
			writeFedErr(w, err)
			return
		}
		labels := []string{}
		if cfg != nil && cfg.Federation != nil {
			labels = cfg.Federation.NodeLabels
		}
		writeJSON(w, 200, map[string]any{"labels": labels})
		return
	}
	var in struct {
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeError(w, 400, "invalid_arg", "expected add/remove label arrays")
		return
	}
	for _, label := range append(append([]string{}, in.Add...), in.Remove...) {
		if !proto.ValidNodeLabel(label) {
			writeError(w, 400, "invalid_arg", "invalid node label: use 1..64 letters, digits, dot, dash or underscore")
			return
		}
	}
	_, err := config.Update(func(cfg *config.Config, loadErr error) error {
		if loadErr != nil {
			return loadErr
		}
		if cfg.Federation == nil {
			cfg.Federation = &config.FederationConfig{}
		}
		set := map[string]bool{}
		for _, s := range cfg.Federation.NodeLabels {
			set[s] = true
		}
		for _, s := range in.Remove {
			delete(set, s)
		}
		for _, s := range in.Add {
			set[s] = true
		}
		labels := []string{}
		for s := range set {
			labels = append(labels, s)
		}
		labels, e := proto.NormalizeNodeLabels(labels)
		if e != nil {
			return e
		}
		cfg.Federation.NodeLabels = labels
		return nil
	})
	if err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return
	}
	broadcastFederationCatalogs()
	if rt := currentFederation(); rt != nil {
		rt.wakeNodes()
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
