package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"text/tabwriter"
	"time"
)

// allNodesGroup is one row of `groups ls --all-nodes`, the CLI side of the
// dashboard's merged "Groups · all nodes" view: this node's groups plus every
// trusted peer's shared groups, named group@node.
type allNodesGroup struct {
	Name    string `json:"name"`
	Group   string `json:"group"`
	Node    string `json:"node"`
	NodeID  string `json:"node_id"`
	Local   bool   `json:"local,omitempty"`
	Members int    `json:"members"`
	Online  int    `json:"online"`
}

type allNodesNode struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Local bool   `json:"local,omitempty"`
	Error string `json:"error,omitempty"`
}

type allNodesStatus struct {
	InstanceID string `json:"instance_id"`
	Name       string `json:"name"`
	Peers      []struct {
		InstanceID string `json:"instance_id"`
		Label      string `json:"label"`
		Name       string `json:"name"`
		Trusted    bool   `json:"trusted"`
	} `json:"peers"`
}

const allNodesConcurrency = 4

// allNodesReaders are the daemon reads behind --all-nodes, swappable in tests.
var allNodesReaders = struct {
	status   func(out any) error
	local    func(out any) error
	peerView func(node, endpoint string, out any) error
}{
	status:   func(out any) error { return DaemonGet("/v1/federation/status", out) },
	local:    func(out any) error { return DaemonGet("/v1/groups", out) },
	peerView: ReadPeerView,
}

func runGroupsLsAllNodes(asJSON bool, stdout, stderr io.Writer) int {
	var st allNodesStatus
	if err := allNodesReaders.status(&st); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	self := st.Name
	if self == "" {
		self = "this node"
	}
	nodes := []allNodesNode{{ID: st.InstanceID, Name: self, Local: true}}
	var rows []allNodesGroup
	var local []groupSummary
	if err := allNodesReaders.local(&local); err != nil {
		nodes[0].Error = err.Error()
	}
	for _, g := range local {
		rows = append(rows, allNodesGroup{Name: g.Name + "@" + self, Group: g.Name, Node: self, NodeID: st.InstanceID, Local: true, Members: g.Members, Online: g.Online})
	}

	// Peers are read through the pinned peer-view transport, at most
	// allNodesConcurrency at a time like the summary listing; an unreachable
	// peer is reported with the rows that did load, and makes the exit status
	// nonzero.
	type peerResult struct {
		node allNodesNode
		rows []allNodesGroup
	}
	var trusted []allNodesNode
	for _, p := range st.Peers {
		if !p.Trusted || p.InstanceID == "" {
			continue
		}
		name := p.Label
		if name == "" {
			name = p.Name
		}
		if name == "" {
			name = p.InstanceID
		}
		trusted = append(trusted, allNodesNode{ID: p.InstanceID, Name: name})
	}
	results := make([]peerResult, len(trusted))
	var wg sync.WaitGroup
	slots := make(chan struct{}, allNodesConcurrency)
	for i, n := range trusted {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			res := peerResult{node: n}
			var snap struct {
				Groups []struct {
					Name    string            `json:"name"`
					Members []json.RawMessage `json:"members"`
					Online  int               `json:"online"`
				} `json:"groups"`
			}
			if err := allNodesReaders.peerView(n.ID, "snapshot", &snap); err != nil {
				res.node.Error = err.Error()
			}
			for _, g := range snap.Groups {
				res.rows = append(res.rows, allNodesGroup{Name: g.Name + "@" + n.Name, Group: g.Name, Node: n.Name, NodeID: n.ID, Members: len(g.Members), Online: g.Online})
			}
			results[i] = res
		}()
	}
	wg.Wait()
	for _, r := range results {
		nodes = append(nodes, r.node)
		rows = append(rows, r.rows...)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Local != rows[j].Local {
			return rows[i].Local
		}
		if rows[i].Node != rows[j].Node {
			return rows[i].Node < rows[j].Node
		}
		return rows[i].Group < rows[j].Group
	})
	if rows == nil {
		rows = []allNodesGroup{}
	}

	rc := rcOK
	for _, n := range nodes {
		if n.Error != "" {
			rc = 1
		}
	}
	if asJSON {
		if err := json.NewEncoder(stdout).Encode(map[string]any{"nodes": nodes, "groups": rows, "read_at": time.Now().UTC()}); err != nil {
			return rcIOFailure
		}
		return rc
	}
	fmt.Fprintln(stdout, "Peer groups are each peer's authorized projection; zero counts may be withheld.")
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "GROUP@NODE\tMEMBERS\tONLINE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%d\t%d\n", peerViewCell(r.Name), r.Members, r.Online)
	}
	_ = tw.Flush()
	for _, n := range nodes {
		if n.Error != "" {
			fmt.Fprintf(stdout, "%s: unreachable (%s)\n", peerViewCell(n.Name), peerViewCell(n.Error))
		}
	}
	return rc
}
