package proto

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const KindNodeUpdate = "node_update"

// NodeMetadata is an instance-wide, path-free advertisement, not a resource reservation.
type NodeMetadata struct {
	Schema         int           `json:"schema"`
	OS             string        `json:"os"`
	OSVersion      string        `json:"os_version"`
	Arch           string        `json:"arch"`
	TclaudeVersion string        `json:"tclaude_version"`
	Harnesses      []NodeHarness `json:"harnesses"`
	Labels         []string      `json:"labels"`
	MaxLiveAgents  int           `json:"max_live_agents"` // zero is unlimited
	Resources      NodeResources `json:"resources"`
}
type NodeHarness struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}
type NodeCPU struct {
	LogicalCores int         `json:"logical_cores"`
	LoadAverage  *[3]float64 `json:"load_average"`
}
type NodeRAM struct {
	TotalBytes         uint64 `json:"total_bytes"`
	AvailableBytes     uint64 `json:"available_bytes"`
	AvailableEstimated bool   `json:"available_estimated"`
}
type NodeDisk struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}
type NodeAgents struct {
	LiveAgents   int `json:"live_agents"`
	LiveSessions int `json:"live_sessions"`
}
type NodeResources struct {
	ObservedAt                  *time.Time  `json:"observed_at"`
	Status                      string      `json:"status"`
	CPU                         NodeCPU     `json:"cpu"`
	RAM                         *NodeRAM    `json:"ram"`
	DataDisk                    *NodeDisk   `json:"data_disk"`
	WorkDiskMinAvailableBytes   *uint64     `json:"work_disk_min_available_bytes"`
	WorkDiskMinAvailablePercent *float64    `json:"work_disk_min_available_percent"`
	Agents                      *NodeAgents `json:"agents"`
}

// At orders publications, including withdrawals. ReceivedAt is receiver-owned
// cache state in CatalogPayload; it must never be trusted from a peer.
type NodeUpdatePayload struct {
	Node *NodeMetadata `json:"node"`
	At   time.Time     `json:"at"`
}

func ValidNodeLabel(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		allowed := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
		if !allowed {
			return false
		}
	}
	return true
}
func NormalizeNodeLabels(in []string) ([]string, error) {
	set := map[string]bool{}
	for _, s := range in {
		if !ValidNodeLabel(s) {
			return nil, fmt.Errorf("invalid node label %q: use 1..64 letters, digits, dot, dash or underscore", s)
		}
		set[s] = true
	}
	if len(set) > 64 {
		return nil, fmt.Errorf("at most 64 node labels")
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// SanitizeNode drops unknown schema and malformed readings instead of inventing
// usable capacity. Remote display values can never drive terminal controls.
func SanitizeNode(n *NodeMetadata) *NodeMetadata {
	if n == nil || n.Schema != 1 || n.MaxLiveAgents < 0 {
		return nil
	}
	n.OS = SafeName(n.OS, false)
	n.Arch = SafeName(n.Arch, false)
	n.OSVersion = safeOptional(n.OSVersion)
	n.TclaudeVersion = safeOptional(n.TclaudeVersion)
	hs := make([]NodeHarness, 0)
	seen := map[string]bool{}
	for _, h := range n.Harnesses {
		if len(hs) >= 16 {
			break
		}
		if !ValidNodeLabel(h.Name) || seen[h.Name] {
			continue
		}
		seen[h.Name] = true
		h.Version = safeOptional(h.Version)
		hs = append(hs, h)
	}
	n.Harnesses = hs
	labels := []string{}
	for _, s := range n.Labels {
		if len(labels) >= 64 {
			break
		}
		if ValidNodeLabel(s) {
			labels = append(labels, s)
		}
	}
	n.Labels, _ = NormalizeNodeLabels(labels)
	r := &n.Resources
	if r.Status != "current" && r.Status != "stale" && r.Status != "warming" {
		r.Status = "stale"
	}
	if r.ObservedAt == nil || r.ObservedAt.IsZero() {
		r.ObservedAt = nil
		r.Status = "warming"
	}
	if r.CPU.LogicalCores < 0 {
		r.CPU.LogicalCores = 0
	}
	if r.CPU.LoadAverage != nil {
		for _, x := range r.CPU.LoadAverage {
			if x < 0 || math.IsInf(x, 0) || math.IsNaN(x) {
				r.CPU.LoadAverage = nil
				break
			}
		}
	}
	if r.RAM != nil && (r.RAM.TotalBytes == 0 || r.RAM.AvailableBytes > r.RAM.TotalBytes) {
		r.RAM = nil
	}
	if r.DataDisk != nil && (r.DataDisk.TotalBytes == 0 || r.DataDisk.AvailableBytes > r.DataDisk.TotalBytes) {
		r.DataDisk = nil
	}
	if p := r.WorkDiskMinAvailablePercent; p != nil && (*p < 0 || *p > 100 || math.IsNaN(*p) || math.IsInf(*p, 0)) {
		r.WorkDiskMinAvailablePercent = nil
	}
	if r.Agents != nil && (r.Agents.LiveAgents < 0 || r.Agents.LiveSessions < 0) {
		r.Agents = nil
	}
	return n
}

type NodeMatch struct{ clauses [][2]string }

func ParseNodeMatch(s string) (NodeMatch, error) {
	var m NodeMatch
	if strings.TrimSpace(s) == "" {
		return m, nil
	}
	for _, c := range strings.Split(s, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(c), "=")
		if !ok || value == "" {
			return m, fmt.Errorf("match must use os=, arch=, label= or harness=")
		}
		switch key {
		case "os", "arch", "label", "harness":
		default:
			return m, fmt.Errorf("unknown node match %q", key)
		}
		if !ValidNodeLabel(value) {
			return m, fmt.Errorf("invalid match value %q", value)
		}
		m.clauses = append(m.clauses, [2]string{key, value})
	}
	return m, nil
}
func (m NodeMatch) Matches(n *NodeMetadata) bool {
	if n == nil {
		return false
	}
	for _, c := range m.clauses {
		found := false
		switch c[0] {
		case "os":
			found = n.OS == c[1]
		case "arch":
			found = n.Arch == c[1]
		case "label":
			for _, v := range n.Labels {
				found = found || v == c[1]
			}
		case "harness":
			for _, h := range n.Harnesses {
				found = found || h.Name == c[1]
			}
		}
		if !found {
			return false
		}
	}
	return true
}
