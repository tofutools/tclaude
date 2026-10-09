package agentd

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// groupFederationLink is one way a local group is connected to a federation
// peer, for the Groups tab's linked-group marker. It is local operator data:
// restricted peers never receive it in a projected snapshot.
//
//   - kind "grant", direction "in": the peer holds peer-grant slugs scoped to
//     this group, directly or through a local node pool.
//   - kind "route", direction "out": a member opened the peer's group route,
//     so this group holds a private mirror of it.
//
// Unscoped (all-groups) grants and unrestricted trust are deliberately not
// listed: they cover every group, so a per-group marker would only add noise.
//
// The links describe this node's other trust relationships, so they are never
// served to a peer, not even an unrestricted one (see peerSnapshotCtxKey).
type groupFederationLink struct {
	Peer      string     `json:"peer"`
	Label     string     `json:"label"`
	Level     string     `json:"level"`
	Kind      string     `json:"kind"`
	Direction string     `json:"direction"`
	Slugs     []string   `json:"slugs,omitempty"`
	Pool      string     `json:"pool,omitempty"`
	Remote    string     `json:"remote,omitempty"`
	Online    bool       `json:"online"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
}

// gatherGroupFederationLinks returns the links of every local group keyed by
// group ID. It costs one peers read when federation has no trusted peers.
func gatherGroupFederationLinks() map[int64][]groupFederationLink {
	peers, err := db.ListFederationPeers()
	if err != nil || len(peers) == 0 {
		return nil
	}
	byID := make(map[string]*db.FederationPeer, len(peers))
	for i := range peers {
		byID[peers[i].InstanceID] = &peers[i]
	}
	dir := map[string]proto.DirectoryEntry{}
	if rt := currentFederation(); rt != nil {
		for _, e := range rt.cl.Directory() {
			dir[e.InstanceID] = e
		}
	}
	base := func(p *db.FederationPeer) groupFederationLink {
		l := groupFederationLink{Peer: p.InstanceID, Label: peerDisplay(p), Level: p.TrustLevel}
		if e, ok := dir[p.InstanceID]; ok {
			l.Online = e.Online
			if !e.LastSeen.IsZero() {
				seen := e.LastSeen
				l.LastSeen = &seen
			}
		}
		return l
	}

	type grantKey struct {
		group      int64
		peer, pool string
	}
	grants := map[grantKey][]string{}
	addGrant := func(peer, pool string, g db.FederationPeerGrant) {
		id, ok := groupIDFromFederationScope(g.Scope)
		if !ok || byID[peer] == nil {
			return
		}
		k := grantKey{id, peer, pool}
		grants[k] = append(grants[k], g.Slug)
	}
	if direct, err := db.ListFederationPeerGrants(""); err == nil {
		for _, g := range direct {
			addGrant(g.Peer, "", g)
		}
	}
	if pools, err := db.ListFederationNodeGroups(); err == nil {
		for _, pool := range pools {
			poolGrants, err := db.ListFederationNodeGroupGrants(pool.ID)
			if err != nil || len(poolGrants) == 0 {
				continue
			}
			members, err := db.ListFederationNodeGroupMembers(pool.ID)
			if err != nil {
				continue
			}
			for _, m := range members {
				for _, g := range poolGrants {
					addGrant(m.InstanceID, pool.Name, g)
				}
			}
		}
	}

	out := map[int64][]groupFederationLink{}
	for k, slugs := range grants {
		l := base(byID[k.peer])
		l.Kind, l.Direction, l.Pool = "grant", "in", k.pool
		sort.Strings(slugs)
		l.Slugs = slugs
		out[k.group] = append(out[k.group], l)
	}
	if mirrors, err := db.ListFederationRouteMirrors(); err == nil {
		// Several members opening the same peer route each get a mirror; the
		// group is linked once.
		type mirrorKey struct {
			group        int64
			peer, remote string
		}
		seen := map[mirrorKey]bool{}
		for _, m := range mirrors {
			p := byID[m.Peer]
			k := mirrorKey{m.GroupID, m.Peer, m.RemoteRoute}
			if p == nil || seen[k] {
				continue
			}
			seen[k] = true
			l := base(p)
			l.Kind, l.Direction = "route", "out"
			l.Remote = m.RemoteLabel
			if l.Remote == "" {
				l.Remote = m.RemoteRoute
			}
			out[m.GroupID] = append(out[m.GroupID], l)
		}
	}
	for id := range out {
		links := out[id]
		sort.Slice(links, func(i, j int) bool {
			a, b := links[i], links[j]
			if a.Label != b.Label {
				return a.Label < b.Label
			}
			if a.Kind != b.Kind {
				return a.Kind < b.Kind
			}
			if a.Pool != b.Pool {
				return a.Pool < b.Pool
			}
			return a.Remote < b.Remote
		})
	}
	return out
}

// peerSnapshotCtxKey marks a dashboard snapshot built for a federation peer
// (servePeerSnapshot) rather than for this node's operator.
type peerSnapshotCtxKey struct{}

func isPeerSnapshot(r *http.Request) bool { return r.Context().Value(peerSnapshotCtxKey{}) != nil }

// groupIDFromFederationScope parses the stored "group=<id>" peer-grant scope.
// An empty (unscoped) scope covers every group and is not a per-group link.
func groupIDFromFederationScope(scope string) (int64, bool) {
	raw, ok := strings.CutPrefix(scope, "group=")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, err == nil
}
