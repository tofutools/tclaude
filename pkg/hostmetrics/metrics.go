// Package hostmetrics reads local resource observations without cgo. It does
// not decide placement or publish local paths to federation peers.
package hostmetrics

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"
)

type CPU struct {
	LogicalCores int         `json:"logical_cores"`
	LoadAverage  *[3]float64 `json:"load_average,omitempty"` // 1, 5 and 15 minutes; not CPU percent
}
type Memory struct {
	TotalBytes         uint64 `json:"total_bytes"`
	AvailableBytes     uint64 `json:"available_bytes"`
	AvailableEstimated bool   `json:"available_estimated,omitempty"`
}
type Path struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}
type Disk struct {
	Path
	MeasuredPath   string `json:"measured_path,omitempty"`
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"` // blocks available to an unprivileged writer
	Error          string `json:"error,omitempty"`
}
type AgentLoad struct {
	LiveAgents   int `json:"live_agents"`
	LiveSessions int `json:"live_sessions"`
}
type Snapshot struct {
	ObservedAt time.Time  `json:"observed_at"`
	OS         string     `json:"os"`
	Arch       string     `json:"arch"`
	CPU        CPU        `json:"cpu"`
	RAM        *Memory    `json:"ram,omitempty"`
	Disks      []Disk     `json:"disks"`
	Tclaude    *AgentLoad `json:"tclaude,omitempty"`
	Errors     []string   `json:"errors,omitempty"`
}
type Thresholds struct {
	LoadPerCore          float64 `json:"load_per_core"`
	RAMAvailablePercent  float64 `json:"ram_available_percent"`
	DiskAvailablePercent float64 `json:"disk_available_percent"`
}
type Warning struct {
	Code     string `json:"code"`
	Resource string `json:"resource"`
	Message  string `json:"message"`
}

func Read(paths []Path) Snapshot {
	// Stamp before collection: a slow filesystem probe must not make CPU/RAM
	// observed earlier in this sample appear fresh when collection completes.
	s := Snapshot{ObservedAt: time.Now().UTC(), OS: runtime.GOOS, Arch: runtime.GOARCH, CPU: CPU{LogicalCores: runtime.NumCPU()}, Disks: []Disk{}}
	load, err := readLoad()
	if err != nil {
		s.Errors = append(s.Errors, "CPU load: "+err.Error())
	} else {
		s.CPU.LoadAverage = &load
	}
	s.RAM, err = readMemory()
	if err != nil {
		s.Errors = append(s.Errors, "RAM: "+err.Error())
	}
	// Stable ordering makes status easy to compare. Paths may refer to future
	// work directories; measure their nearest existing ancestor without creating them.
	paths = append([]Path(nil), paths...)
	sort.Slice(paths, func(i, j int) bool {
		if paths[i].Kind == paths[j].Kind {
			return paths[i].Path < paths[j].Path
		}
		return paths[i].Kind < paths[j].Kind
	})
	seen := map[string]bool{}
	for _, p := range paths {
		if p.Path == "" {
			continue
		}
		abs, e := filepath.Abs(p.Path)
		if e != nil {
			s.Disks = append(s.Disks, Disk{Path: p, Error: e.Error()})
			continue
		}
		p.Path = abs
		key := p.Kind + "\x00" + abs
		if seen[key] {
			continue
		}
		seen[key] = true
		d := Disk{Path: p}
		d.MeasuredPath = abs
		for {
			_, e = os.Stat(d.MeasuredPath)
			if e == nil || !os.IsNotExist(e) {
				break
			}
			parent := filepath.Dir(d.MeasuredPath)
			if parent == d.MeasuredPath {
				break
			}
			d.MeasuredPath = parent
		}
		if e == nil {
			d.TotalBytes, d.AvailableBytes, e = readDisk(d.MeasuredPath)
		}
		if e != nil {
			d.Error = e.Error()
		}
		s.Disks = append(s.Disks, d)
	}
	return s
}

func Warnings(s Snapshot, t Thresholds) []Warning {
	out := []Warning{}
	if t.LoadPerCore > 0 && s.CPU.LogicalCores > 0 && s.CPU.LoadAverage != nil {
		n := s.CPU.LoadAverage[0] / float64(s.CPU.LogicalCores)
		if n > t.LoadPerCore {
			out = append(out, Warning{"high_load", "cpu", fmt.Sprintf("1m load %.2f per logical core exceeds %.2f", n, t.LoadPerCore)})
		}
	}
	if s.RAM != nil && s.RAM.TotalBytes > 0 && t.RAMAvailablePercent > 0 {
		pct := 100 * float64(s.RAM.AvailableBytes) / float64(s.RAM.TotalBytes)
		if pct < t.RAMAvailablePercent {
			out = append(out, Warning{"low_ram", "ram", fmt.Sprintf("RAM available %.1f%% is below %.1f%%", pct, t.RAMAvailablePercent)})
		}
	}
	for _, d := range s.Disks {
		if d.Error != "" || d.TotalBytes == 0 || t.DiskAvailablePercent <= 0 {
			continue
		}
		pct := 100 * float64(d.AvailableBytes) / float64(d.TotalBytes)
		if pct < t.DiskAvailablePercent {
			out = append(out, Warning{"low_disk", d.Path.Path, fmt.Sprintf("Disk available %.1f%% is below %.1f%%", pct, t.DiskAvailablePercent)})
		}
	}
	return out
}

func multiply(a, b uint64) (uint64, error) {
	if b > 0 && a > math.MaxUint64/b {
		return 0, fmt.Errorf("resource counter overflow")
	}
	return a * b, nil
}
