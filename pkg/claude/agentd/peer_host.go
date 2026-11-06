package agentd

import (
	"net/http"
)

// This node's host status under host.read (unscoped): load, memory and disk
// capacity, the live agent count, and the warnings its thresholds raise. A
// restricted peer gets them without host paths: a disk shows its kind (data,
// work, …), never the directory measured, and an error or warning only what
// it concerns, never its message, which may name a path. Unrestricted trust
// reads the status as this node's operator does (GET /v1/host/status).

func addPeerHostRules(rules map[string]peerViewRule) {
	rules["GET /api/host/status"] = peerViewRule{feature: "host", requires: PermHostRead, serve: servePeerHostStatus}
}

type peerHostDisk struct {
	Kind           string `json:"kind"`
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	Unavailable    bool   `json:"unavailable,omitempty"`
}

type peerHostWarning struct {
	Code     string `json:"code"`
	Resource string `json:"resource"`
}

func servePeerHostStatus(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if !v.allows(rule, 0) {
		writeError(w, http.StatusForbidden, "permission", "requires "+rule.requires)
		return
	}
	h := cachedHostStatus()
	if v.unrestricted() {
		writeJSON(w, http.StatusOK, h)
		return
	}
	disks := make([]peerHostDisk, 0, len(h.Disks))
	kindOf := map[string]string{}
	for _, d := range h.Disks {
		disks = append(disks, peerHostDisk{Kind: d.Kind, TotalBytes: d.TotalBytes, AvailableBytes: d.AvailableBytes, Unavailable: d.Error != ""})
		kindOf[d.Path.Path] = d.Kind
	}
	warnings := make([]peerHostWarning, 0, len(h.Warnings))
	for _, warn := range h.Warnings {
		// A disk warning names the disk by its path: the peer gets its kind.
		resource := warn.Resource
		if resource != "cpu" && resource != "ram" {
			resource = "disk"
			if kind := kindOf[warn.Resource]; kind != "" {
				resource = "disk:" + kind
			}
		}
		warnings = append(warnings, peerHostWarning{Code: warn.Code, Resource: resource})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"observed_at": h.ObservedAt, "status": h.Status, "age_seconds": h.AgeSeconds,
		"sample_interval_seconds": h.SampleIntervalSeconds, "os": h.OS, "arch": h.Arch,
		"cpu": h.CPU, "ram": h.RAM, "tclaude": h.Tclaude, "disks": disks,
		"thresholds": h.Thresholds, "warnings": warnings, "errors": len(h.Errors),
	})
}
